package gitsource

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/catalog"
)

// testRepo commits the PoC catalog under catalog/ in a new repository and returns its path
// and commit.
func testRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir = t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "catalog"), os.DirFS("../testdata/poc")); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(append(os.Environ(), gitEnv...), "GIT_CONFIG_GLOBAL="+os.DevNull)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	run("init", "--quiet", "--initial-branch=main")
	run("add", ".")
	run("commit", "--quiet", "-m", "catalog")
	sha = run("rev-parse", "HEAD")
	return dir, sha[:len(sha)-1]
}

func TestLoad(t *testing.T) {
	repo, sha := testRepo(t)
	ctx := context.Background()
	url := "file://" + repo

	c, errs, err := Load(ctx, url, "main", "catalog", catalog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || catalog.HasErrors(errs) {
		t.Fatalf("catalog %v, diagnostics %v", c != nil, errs)
	}
	if c.SHA != sha {
		t.Errorf("SHA = %q, want the commit %q", c.SHA, sha)
	}

	tests := []struct {
		name, url, ref, dir string
		gitErr              bool
	}{
		{"unknown ref", url, "nope", "catalog", true},
		{"no repository", "file://" + filepath.Join(repo, "missing"), "main", "catalog", true},
		{"ext transport refused", "ext::sh -c touch% /tmp/pwned", "main", "catalog", true},
		{"missing directory", url, "main", "elsewhere", false},
		{"directory outside the repository", url, "main", "../..", false},
		{"no ref", url, "", "catalog", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(ctx, tt.url, tt.ref, tt.dir, catalog.Options{})
			if err == nil {
				t.Fatal("want an error")
			}
			if errors.Is(err, ErrGit) != tt.gitErr {
				t.Errorf("errors.Is(err, ErrGit) = %v, want %v: %v", !tt.gitErr, tt.gitErr, err)
			}
		})
	}
}

func TestLoadHonoursCancellation(t *testing.T) {
	repo, _ := testRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := Load(ctx, "file://"+repo, "main", "catalog", catalog.Options{}); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
}

// TestLoadRefusesSymlinkEscape: a catalog directory that is a symlink out of the repository
// (e.g. to the dispatcher host's deploy/poc) must not be read.
func TestLoadRefusesSymlinkEscape(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	outside := t.TempDir()
	if err := os.CopyFS(outside, os.DirFS("../testdata/poc")); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "catalog")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"add", "."},
		{"commit", "--quiet", "-m", "symlinked catalog"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Env = append(append(os.Environ(), gitEnv...), "GIT_CONFIG_GLOBAL="+os.DevNull)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	c, _, err := Load(context.Background(), "file://"+dir, "main", "catalog", catalog.Options{})
	if err == nil || c != nil {
		t.Fatalf("Load followed a symlink out of the checkout: catalog %v, err %v", c != nil, err)
	}
}

// gitIn runs git in dir with a neutral configuration and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
	cmd.Env = append(append(os.Environ(), gitEnv...), "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestHead(t *testing.T) {
	ctx := context.Background()
	repo, sha := testRepo(t)
	url := "file://" + repo
	gitIn(t, repo, "tag", "-a", "-m", "release", "v1")
	gitIn(t, repo, "tag", "light")
	gitIn(t, repo, "commit", "--quiet", "--allow-empty", "-m", "next")
	next := gitIn(t, repo, "rev-parse", "HEAD")

	tests := []struct {
		name, url, ref, want string
		gitErr               bool
	}{
		{"branch", url, "main", next, false},
		{"full branch ref", url, "refs/heads/main", next, false},
		{"annotated tag peeled to its commit", url, "v1", sha, false},
		{"lightweight tag", url, "light", sha, false},
		{"pinned commit", url, sha, sha, false},
		{"pinned commit needs no repository", "file:///nonexistent", sha, sha, false},
		{"unknown ref", url, "nope", "", true},
		{"no repository", "file://" + filepath.Join(repo, "missing"), "main", "", true},
		{"ext transport refused", "ext::sh -c touch% /tmp/pwned", "main", "", true},
		{"no ref", url, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Head(ctx, tt.url, tt.ref)
			if tt.gitErr {
				if !errors.Is(err, ErrGit) {
					t.Fatalf("Head = %q, %v; want an ErrGit", got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Head = %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Head(cctx, url, "main"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Head(cancelled) = %v, want context.Canceled", err)
	}
}

// TestErrorsRedactCredentials: a token in the repository URL never reaches an error.
func TestErrorsRedactCredentials(t *testing.T) {
	ctx := context.Background()
	repoDir, _ := testRepo(t)
	// File URLs with user info: git ignores it, so these reach the repository (or fail on a
	// missing one) with no network, and the unknown-ref error names the URL.
	withToken := "file://x-access-token:s3cr3t@" + repoDir
	missing := "file://x-access-token:s3cr3t@" + filepath.Join(t.TempDir(), "missing")
	_, headErr := Head(ctx, withToken, "nope")
	if headErr == nil || !strings.Contains(headErr.Error(), "redacted@") {
		t.Fatalf("Head(unknown ref) = %v, want the redacted URL in the message", headErr)
	}
	_, _, loadErr := Load(ctx, missing, "main", "catalog", catalog.Options{})
	for name, err := range map[string]error{"Head": headErr, "Load": loadErr} {
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		if strings.Contains(err.Error(), "s3cr3t") {
			t.Errorf("%s error has the token: %v", name, err)
		}
		if !errors.Is(err, ErrGit) {
			t.Errorf("%s error doesn't wrap ErrGit after redaction: %v", name, err)
		}
	}

	const repo = "https://x-access-token:s3cr3t@github.com/compliance-framework/factory"
	tests := []struct{ in, want string }{
		{repo, "https://redacted@github.com/compliance-framework/factory"},
		{"https://github.com/compliance-framework/factory", "https://github.com/compliance-framework/factory"},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git"},
	}
	for _, tt := range tests {
		if got := RedactURL(tt.in); got != tt.want {
			t.Errorf("RedactURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	base := errors.New("fatal: unable to access '" + repo + "/': x-access-token:s3cr3t@ refused")
	err := redactError(base, repo)
	if strings.Contains(err.Error(), "s3cr3t") || !errors.Is(err, base) {
		t.Errorf("redactError = %v (unwraps: %v)", err, errors.Is(err, base))
	}
}
