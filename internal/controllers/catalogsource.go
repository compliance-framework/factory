package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/compliance-framework/factory/internal/catalog"
	"github.com/compliance-framework/factory/internal/catalog/gitsource"
)

// CatalogSource is where catalog-sync reads the catalog from.
type CatalogSource interface {
	// Head returns the revision the source is at now: for git, the commit the configured
	// ref points to (git ls-remote); for a directory, a hash of its files. It must be cheap
	// enough to call on every poll.
	Head(ctx context.Context) (string, error)
	// Load loads and validates the catalog at revision (as returned by Head). The snapshot's
	// SHA is revision. The diagnostics are catalog.LoadDir's; err is set only when the
	// revision couldn't be read at all.
	Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error)
	// String describes the source for logs and status, without credentials.
	String() string
}

// Catalog-sync source defaults (the LLD's --catalog-* flags).
const (
	DefaultCatalogRepo = "https://github.com/compliance-framework/factory"
	DefaultCatalogRef  = "main"
	DefaultCatalogPath = "catalog"
)

// GitSource reads the catalog from a git repository through gitsource: Head is
// gitsource.Head (git ls-remote), Load is gitsource.Load at that commit (a shallow fetch into
// a temporary directory). gitsource redacts credentials in the URL from its errors.
type GitSource struct {
	// Repo is the repository URL (https, ssh, git or file). Default DefaultCatalogRepo.
	Repo string
	// Ref is the branch or tag to follow, or a full commit SHA to pin. Default "main".
	Ref string
	// Path is the catalog's directory in the repository. Default "catalog".
	Path string
	// Options are passed to catalog.LoadDir (SHA is set by gitsource).
	Options catalog.Options
}

var _ CatalogSource = GitSource{}

func (g GitSource) withDefaults() GitSource {
	if g.Repo == "" {
		g.Repo = DefaultCatalogRepo
	}
	if g.Ref == "" {
		g.Ref = DefaultCatalogRef
	}
	if g.Path == "" {
		g.Path = DefaultCatalogPath
	}
	return g
}

func (g GitSource) String() string {
	g = g.withDefaults()
	return fmt.Sprintf("git %s ref %s path %s", gitsource.RedactURL(g.Repo), g.Ref, g.Path)
}

// Head resolves Ref with gitsource.Head (git ls-remote): a branch wins over a tag, an
// annotated tag resolves to its commit, and a full commit SHA is pinned.
func (g GitSource) Head(ctx context.Context) (string, error) {
	g = g.withDefaults()
	return gitsource.Head(ctx, g.Repo, g.Ref)
}

// Load fetches revision and loads the catalog under Path (gitsource.Load).
func (g GitSource) Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error) {
	g = g.withDefaults()
	return gitsource.Load(ctx, g.Repo, revision, g.Path, g.Options)
}

// DirSource reads the catalog from a local directory (`--catalog-dir`, local development).
// Head hashes the files LoadDir reads (the same "dir:<sha256>" LoadDir computes), so an edit
// is picked up on the next poll.
type DirSource struct {
	// FS is the catalog root, e.g. os.DirFS(dir).
	FS fs.FS
	// Name describes the directory in logs and status.
	Name string
	// Options are passed to catalog.LoadDir (SHA is set to the revision).
	Options catalog.Options
}

var _ CatalogSource = DirSource{}

// NewDirSource returns a DirSource over the directory dir.
func NewDirSource(dir string, opts catalog.Options) DirSource {
	return DirSource{FS: os.DirFS(dir), Name: dir, Options: opts}
}

func (d DirSource) String() string { return "dir " + d.Name }

// Head hashes the directory: every regular file outside dot-directories that isn't a dot
// file, in lexical order (path, length and content), as LoadDir does.
func (d DirSource) Head(ctx context.Context) (string, error) {
	if d.FS == nil {
		return "", errors.New("dir source: no FS")
	}
	sum := sha256.New()
	err := fs.WalkDir(d.FS, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if de.IsDir() {
			if p != "." && strings.HasPrefix(de.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !de.Type().IsRegular() || strings.HasPrefix(de.Name(), ".") {
			return nil
		}
		data, err := fs.ReadFile(d.FS, p)
		if err != nil {
			return err
		}
		fmt.Fprintf(sum, "%s\x00%d\x00", p, len(data))
		sum.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("dir source %s: %w", d.Name, err)
	}
	return "dir:" + hex.EncodeToString(sum.Sum(nil)), nil
}

// Load runs catalog.LoadDir on the directory, labelling the snapshot with revision.
func (d DirSource) Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error) {
	if d.FS == nil {
		return nil, nil, errors.New("dir source: no FS")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	opts := d.Options
	opts.SHA = revision
	c, errs := catalog.LoadDir(d.FS, opts)
	return c, errs, nil
}
