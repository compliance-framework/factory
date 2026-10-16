package provider

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Redacted replaces a token wherever a Credential is printed, logged or JSON-encoded.
const Redacted = "[REDACTED]"

// Credential is a minted git token, the API's GitCredential. It is sent to the agent in the
// lease response and never stored.
//
// Printing (fmt, any verb), logging (slog) and JSON-encoding it all redact Token, so it can't
// leak through a log line or a store record. The lease response copies the fields into the
// generated API type explicitly.
type Credential struct {
	Provider string
	// Host is the git host ("github.com"); Username goes with the token ("x-access-token").
	Host        string
	Username    string
	Token       string
	ExpiresAt   time.Time
	Repo        string
	ReadRepos   []string
	Permissions map[string]string
}

var (
	_ fmt.Stringer   = Credential{}
	_ fmt.Formatter  = Credential{}
	_ slog.LogValuer = Credential{}
	_ json.Marshaler = Credential{}
)

// redactedToken is what replaces the token: Redacted, or "" when there is no token.
func (c Credential) redactedToken() string {
	if c.Token == "" {
		return ""
	}
	return Redacted
}

// String describes the credential with the token redacted.
func (c Credential) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Credential{provider=%s host=%s username=%s token=%s expiresAt=%s repo=%s",
		c.Provider, c.Host, c.Username, c.redactedToken(), c.ExpiresAt.Format(time.RFC3339), c.Repo)
	if len(c.ReadRepos) > 0 {
		fmt.Fprintf(&b, " readRepos=%v", c.ReadRepos)
	}
	if len(c.Permissions) > 0 {
		fmt.Fprintf(&b, " permissions=%v", c.Permissions)
	}
	b.WriteString("}")
	return b.String()
}

// Format prints String() for every verb, %#v and %d included, so no verb reaches the token.
func (c Credential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// LogValue logs the credential's fields with the token redacted.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", c.Provider),
		slog.String("host", c.Host),
		slog.String("username", c.Username),
		slog.String("token", c.redactedToken()),
		slog.Time("expiresAt", c.ExpiresAt),
		slog.String("repo", c.Repo),
		slog.Any("readRepos", c.ReadRepos),
		slog.Any("permissions", c.Permissions),
	)
}

// MarshalJSON encodes the credential with the token redacted.
func (c Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Provider    string            `json:"provider"`
		Host        string            `json:"host"`
		Username    string            `json:"username"`
		Token       string            `json:"token"`
		ExpiresAt   time.Time         `json:"expiresAt"`
		Repo        string            `json:"repo"`
		ReadRepos   []string          `json:"readRepos,omitzero"`
		Permissions map[string]string `json:"permissions,omitzero"`
	}{c.Provider, c.Host, c.Username, c.redactedToken(), c.ExpiresAt, c.Repo, c.ReadRepos, c.Permissions})
}
