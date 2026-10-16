package controllers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compliance-framework/factory/internal/catalog"
)

const pocCatalog = "../catalog/testdata/poc"

// brokenRule is a file that fails the catalog: invalid YAML.
const brokenRule = "apiVersion: factory.ccf.dev/v1alpha1\nkind: Rule\nmetadata: [unclosed\n"

func hasCode(errs []catalog.Error, code, file string) bool {
	return slices.ContainsFunc(errs, func(e catalog.Error) bool { return e.Code == code && e.File == file })
}

// TestCatalogSyncDirEndToEnd runs catalog-sync over the real loader on a copy of the PoC
// catalog: good, then corrupted (kept old), then repaired.
func TestCatalogSyncDirEndToEnd(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(pocCatalog)); err != nil {
		t.Fatal(err)
	}
	src := NewDirSource(dir, catalog.Options{})
	s, clk := newTestSync(t, src, CatalogSyncOptions{})

	if err := s.Sync(ctx, false); err != nil {
		t.Fatalf("Sync(good) = %v; status %+v", err, s.Status())
	}
	good := s.Current()
	if good == nil || len(good.RuleSets) == 0 || !strings.HasPrefix(good.SHA, "dir:") {
		t.Fatalf("live catalog %+v", good)
	}
	// Head hashes exactly what LoadDir hashes.
	if c, errs := catalog.LoadDir(os.DirFS(dir), catalog.Options{}); c == nil || c.SHA != good.SHA {
		t.Fatalf("LoadDir's own SHA %v differs from Head's %s (%v)", c, good.SHA, errs)
	}

	broken := filepath.Join(dir, "rules", "zz-broken.yaml")
	if err := os.WriteFile(broken, []byte(brokenRule), 0o600); err != nil {
		t.Fatal(err)
	}
	t1 := clk.Advance(DefaultCatalogPollInterval)
	err := s.Sync(ctx, false)
	if !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(broken) = %v, want ErrCatalogInvalid", err)
	}
	if s.Current() != good {
		t.Fatal("the live catalog changed after a bad load")
	}
	st := s.Status()
	if st.LastGoodSHA != good.SHA || st.LastAttemptSHA == good.SHA || !strings.HasPrefix(st.LastAttemptSHA, "dir:") ||
		!st.LastFailure.Equal(t1) || !st.LastSuccess.Equal(t0) || !hasCode(st.Errors, catalog.CodeSyntax, "rules/zz-broken.yaml") {
		t.Fatalf("status = %+v", st)
	}

	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx, false); err != nil {
		t.Fatalf("Sync(repaired) = %v", err)
	}
	if st := s.Status(); s.Current() != good || st.LastAttemptSHA != good.SHA || st.Errors != nil {
		t.Fatalf("after repair (same content as the live catalog): %+v", st)
	}
}

// testRepo commits the PoC catalog under catalog/ in a new repository on main and returns
// its path, a git runner for it, and the commit.
func testRepo(t *testing.T) (dir string, git func(args ...string) string, sha string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir = t.TempDir()
	if err := os.CopyFS(filepath.Join(dir, "catalog"), os.DirFS(pocCatalog)); err != nil {
		t.Fatal(err)
	}
	git = func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=gc.auto", "GIT_CONFIG_VALUE_0=0", "GIT_CONFIG_KEY_1=maintenance.auto", "GIT_CONFIG_VALUE_1=false")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	git("add", ".")
	git("commit", "--quiet", "-m", "catalog")
	return dir, git, git("rev-parse", "HEAD")
}

func TestCatalogSyncGitEndToEnd(t *testing.T) {
	ctx := context.Background()
	repo, git, sha := testRepo(t)
	src := GitSource{Repo: "file://" + repo, Ref: "main", Path: "catalog"}
	s, _ := newTestSync(t, src, CatalogSyncOptions{})

	if err := s.Sync(ctx, false); err != nil {
		t.Fatalf("Sync = %v; status %+v", err, s.Status())
	}
	if c := s.Current(); c == nil || c.SHA != sha {
		t.Fatalf("live catalog %v, want commit %s", c, sha)
	}

	if err := os.WriteFile(filepath.Join(repo, "catalog", "rules", "zz-broken.yaml"), []byte(brokenRule), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "break the catalog")
	bad := git("rev-parse", "HEAD")

	if err := s.Sync(ctx, false); !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("Sync(bad commit) = %v, want ErrCatalogInvalid", err)
	}
	st := s.Status()
	if s.Current().SHA != sha || st.LastGoodSHA != sha || st.LastAttemptSHA != bad || !hasCode(st.Errors, catalog.CodeSyntax, "rules/zz-broken.yaml") {
		t.Fatalf("status = %+v", st)
	}
}

func TestGitSourceRedactsCredentials(t *testing.T) {
	g := GitSource{Repo: "https://x-access-token:s3cr3t@github.com/compliance-framework/factory"}
	if s := g.String(); strings.Contains(s, "s3cr3t") || !strings.Contains(s, "github.com/compliance-framework/factory") {
		t.Errorf("String() = %q", s)
	}
}

func TestDefaults(t *testing.T) {
	g := GitSource{}.withDefaults()
	if g.Repo != DefaultCatalogRepo || g.Ref != "main" || g.Path != "catalog" {
		t.Errorf("GitSource defaults = %+v", g)
	}
	if _, err := (DirSource{}).Head(context.Background()); err == nil {
		t.Error("DirSource without FS: want an error")
	}
}
