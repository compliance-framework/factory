package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/compliance-framework/factory/internal/scheduler"
)

// maxGitHubBody bounds what is read of a GitHub response.
const maxGitHubBody = 1 << 20

// ghUser is what GET /user returns that the factory uses.
type ghUser struct {
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

// userClient calls GitHub's REST API with a user token.
type userClient struct {
	http    *http.Client
	apiBase string
}

// user is GET <apiBase>/user with token. A token GitHub refuses (401, 403) is
// ErrUnauthenticated; any other failure is ErrOAuth. The token is never logged or kept.
func (c *userClient) user(ctx context.Context, token string) (ghUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"/user", nil)
	if err != nil {
		return ghUser{}, fmt.Errorf("github user: %w: %w", ErrOAuth, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return ghUser{}, fmt.Errorf("github user: %w: %w", ErrOAuth, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ghUser{}, fmt.Errorf("github user: token refused (%d): %w", resp.StatusCode, ErrUnauthenticated)
	case resp.StatusCode != http.StatusOK:
		return ghUser{}, fmt.Errorf("github user: status %d: %w", resp.StatusCode, ErrOAuth)
	}
	var u ghUser
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGitHubBody)).Decode(&u); err != nil {
		return ghUser{}, fmt.Errorf("github user: decode: %w: %w", ErrOAuth, err)
	}
	if u.Login == "" {
		return ghUser{}, fmt.Errorf("github user: no login: %w", ErrOAuth)
	}
	return u, nil
}

// redactURLError drops the URL from a transport error (it never carries the token, which
// travels in a header, but keep error strings free of request details anyway).
func redactURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// GitHubOwners is the scheduler's OwnerVerifier: it exchanges an agent owner's GitHub user
// token (from the device flow) for the login with GET /user on APIBase. The token is used for
// that one call and dropped: never logged or stored.
type GitHubOwners struct {
	// Provider is the catalog Provider the logins belong to (Owner.Provider).
	Provider string
	// APIBase defaults to https://api.github.com.
	APIBase string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

var _ scheduler.OwnerVerifier = GitHubOwners{}

// VerifyOwner implements scheduler.OwnerVerifier. A token GitHub refuses wraps
// scheduler.ErrUnauthorized.
func (g GitHubOwners) VerifyOwner(ctx context.Context, token string) (scheduler.Owner, error) {
	if token == "" {
		return scheduler.Owner{}, fmt.Errorf("no owner token: %w", scheduler.ErrUnauthorized)
	}
	base := strings.TrimRight(g.APIBase, "/")
	if base == "" {
		base = DefaultAPIBase
	}
	hc := g.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	u, err := (&userClient{http: hc, apiBase: base}).user(ctx, token)
	if errors.Is(err, ErrUnauthenticated) {
		return scheduler.Owner{}, fmt.Errorf("owner token: %w: %w", scheduler.ErrUnauthorized, err)
	}
	if err != nil {
		return scheduler.Owner{}, err
	}
	return scheduler.Owner{Provider: g.Provider, Login: u.Login}, nil
}

// exchange trades an OAuth code for a user access token at TokenURL.
func (a *Authenticator) exchange(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {a.cfg.OAuth.ClientID},
		"client_secret": {a.cfg.OAuth.ClientSecret},
		"code":          {code},
	}
	if a.cfg.OAuth.RedirectURL != "" {
		form.Set("redirect_uri", a.cfg.OAuth.RedirectURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.OAuth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("oauth exchange: %w: %w", ErrOAuth, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth exchange: %w: %w", ErrOAuth, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth exchange: status %d: %w", resp.StatusCode, ErrOAuth)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxGitHubBody)).Decode(&tr); err != nil {
		return "", fmt.Errorf("oauth exchange: decode: %w: %w", ErrOAuth, err)
	}
	if tr.Error != "" || tr.AccessToken == "" {
		// GitHub answers 200 with an error code (bad_verification_code, …): a code that
		// doesn't exchange is the caller's problem.
		return "", fmt.Errorf("oauth exchange: code refused: %w", ErrUnauthenticated)
	}
	return tr.AccessToken, nil
}
