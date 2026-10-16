package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/provider"
)

const secretToken = "ghs_s3cr3t-token-value"

func testCredential() provider.Credential {
	return provider.Credential{
		Provider:    "github-public",
		Host:        "github.com",
		Username:    "x-access-token",
		Token:       secretToken,
		ExpiresAt:   time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC),
		Repo:        "compliance-framework/docs",
		ReadRepos:   []string{"compliance-framework/api"},
		Permissions: map[string]string{"contents": "write"},
	}
}

func TestCredentialRedacted(t *testing.T) {
	c := testCredential()
	type wrapper struct{ Cred provider.Credential }
	tests := []struct {
		name string
		out  func() string
	}{
		{"String", c.String},
		{"%v", func() string { return fmt.Sprintf("%v", c) }},
		{"%+v", func() string { return fmt.Sprintf("%+v", c) }},
		{"%#v", func() string { return fmt.Sprintf("%#v", c) }},
		{"%s", func() string { return fmt.Sprintf("%s", c) }},
		{"%q", func() string { return fmt.Sprintf("%q", c) }},
		{"%x", func() string { return fmt.Sprintf("%x", c) }},
		{"%d", func() string { return fmt.Sprintf("%d", c) }},
		{"pointer %v", func() string { return fmt.Sprintf("%v", &c) }},
		{"nested %+v", func() string { return fmt.Sprintf("%+v", wrapper{c}) }},
		{"json", func() string { b, _ := json.Marshal(c); return string(b) }},
		{"nested json", func() string { b, _ := json.Marshal(wrapper{c}); return string(b) }},
		{"slog text", func() string { return logged(false, slog.Any("cred", c)) }},
		{"slog json", func() string { return logged(true, slog.Any("cred", c)) }},
		{"slog json nested", func() string { return logged(true, slog.Any("lease", wrapper{c})) }},
		{"slog text nested", func() string { return logged(false, slog.Any("lease", wrapper{c})) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := tt.out()
			if strings.Contains(out, secretToken) {
				t.Fatalf("token leaked: %s", out)
			}
			if !strings.Contains(out, "REDACTED") {
				t.Fatalf("no redaction marker in %s", out)
			}
			if !strings.Contains(out, "compliance-framework/docs") {
				t.Fatalf("repo missing from %s", out)
			}
		})
	}
}

func logged(asJSON bool, attr slog.Attr) string {
	var buf bytes.Buffer
	var h slog.Handler = slog.NewTextHandler(&buf, nil)
	if asJSON {
		h = slog.NewJSONHandler(&buf, nil)
	}
	slog.New(h).LogAttrs(context.Background(), slog.LevelInfo, "minted", attr)
	return buf.String()
}

func TestCredentialEmptyTokenNotMarked(t *testing.T) {
	c := testCredential()
	c.Token = ""
	if strings.Contains(c.String(), provider.Redacted) {
		t.Fatalf("empty token shown as redacted: %s", c)
	}
}

func TestRetryableError(t *testing.T) {
	cause := errors.New("secondary rate limit")
	err := fmt.Errorf("apply comment: %w", &provider.RetryableError{RetryAfter: time.Minute, Err: cause})
	if !errors.Is(err, provider.ErrRetryable) {
		t.Error("errors.Is(err, ErrRetryable) = false")
	}
	if !errors.Is(err, cause) {
		t.Error("errors.Is(err, cause) = false")
	}
	var re *provider.RetryableError
	if !errors.As(err, &re) || re.RetryAfter != time.Minute {
		t.Errorf("errors.As = %v, %+v", errors.As(err, &re), re)
	}
	if errors.Is(err, provider.ErrNotFound) {
		t.Error("retryable error is ErrNotFound")
	}
	if got := (&provider.RetryableError{Err: cause}).Error(); got != "provider: retryable: secondary rate limit" {
		t.Errorf("Error() = %q", got)
	}
}

func TestEventID(t *testing.T) {
	if got := provider.EventID("gh-123", 2); got != "gh-123/2" {
		t.Errorf("EventID = %q", got)
	}
}

func TestStaticGroups(t *testing.T) {
	lookup := provider.StaticGroups(map[string]v1alpha1.GroupSpec{
		"maintainers": {Members: []v1alpha1.GroupSource{{Provider: "gh", Users: []string{"alice"}}}},
	})
	if g, ok := lookup("maintainers"); !ok || g.Members[0].Users[0] != "alice" {
		t.Errorf("lookup(maintainers) = %+v, %v", g, ok)
	}
	if _, ok := lookup("nobody"); ok {
		t.Error("lookup(nobody) found a group")
	}
}

type stubAdapter struct {
	typ string
	err error
}

func (s stubAdapter) Type() string { return s.typ }

func (s stubAdapter) Connect(context.Context, v1alpha1.Provider, provider.SecretResolver) (provider.Conn, error) {
	return nil, s.err
}

func TestRegistry(t *testing.T) {
	r, err := provider.NewRegistry(stubAdapter{typ: "github"})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		adapter provider.Adapter
		want    error
	}{
		{"duplicate", stubAdapter{typ: "github"}, provider.ErrAlreadyRegistered},
		{"empty type", stubAdapter{}, provider.ErrInvalid},
		{"nil", nil, provider.ErrInvalid},
		{"new type", stubAdapter{typ: "slack", err: errors.New("boom")}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := r.Register(tt.adapter); !errors.Is(err, tt.want) {
				t.Fatalf("Register = %v, want %v", err, tt.want)
			}
		})
	}
	if got := r.Types(); strings.Join(got, ",") != "github,slack" {
		t.Errorf("Types = %v", got)
	}
	if a, ok := r.Lookup("github"); !ok || a.Type() != "github" {
		t.Errorf("Lookup(github) = %v, %v", a, ok)
	}
	if _, ok := r.Lookup("gitlab"); ok {
		t.Error("Lookup(gitlab) found an adapter")
	}

	p := v1alpha1.Provider{Metadata: v1alpha1.ObjectMeta{Name: "x"}, Spec: v1alpha1.ProviderSpec{Type: "gitlab"}}
	if _, err := r.Connect(context.Background(), p, nil); !errors.Is(err, provider.ErrUnknownType) {
		t.Errorf("Connect(gitlab) = %v, want ErrUnknownType", err)
	}
	p.Spec.Type = "slack"
	if _, err := r.Connect(context.Background(), p, nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Connect(slack) = %v, want the adapter's error", err)
	}

	var zero provider.Registry
	if err := zero.Register(stubAdapter{typ: "github"}); err != nil {
		t.Errorf("zero Registry Register = %v", err)
	}
}

func TestFuncAdapters(t *testing.T) {
	var got provider.RawEvent
	sink := provider.IngressSinkFunc(func(_ context.Context, ev provider.RawEvent) error { got = ev; return nil })
	if err := sink.Accept(context.Background(), provider.RawEvent{ID: "gh-1", Headers: http.Header{}}); err != nil || got.ID != "gh-1" {
		t.Errorf("IngressSinkFunc: %v, %+v", err, got)
	}
	secrets := provider.SecretResolverFunc(func(_ context.Context, ref v1alpha1.SecretRef) ([]byte, error) {
		return []byte(ref.Name + "/" + ref.Key), nil
	})
	if b, err := secrets.Resolve(context.Background(), v1alpha1.SecretRef{Name: "app", Key: "key"}); err != nil || string(b) != "app/key" {
		t.Errorf("SecretResolverFunc = %q, %v", b, err)
	}
}
