package controllers

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/compliance-framework/factory/internal/catalog"
)

// FakeRevision is what a FakeCatalogSource's Load returns for one revision.
type FakeRevision struct {
	Catalog     *catalog.Catalog
	Diagnostics []catalog.Error
	Err         error
}

// FakeCatalogSource is an in-memory CatalogSource for tests. It is safe for concurrent use.
type FakeCatalogSource struct {
	mu        sync.Mutex
	head      string
	headErr   error
	revisions map[string]FakeRevision
	loads     []string
}

var _ CatalogSource = (*FakeCatalogSource)(nil)

// NewFakeCatalogSource returns a fake with no revisions and an empty head.
func NewFakeCatalogSource() *FakeCatalogSource {
	return &FakeCatalogSource{revisions: map[string]FakeRevision{}}
}

// SetHead sets what Head returns.
func (f *FakeCatalogSource) SetHead(revision string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head, f.headErr = revision, err
}

// Set sets what Load returns for revision.
func (f *FakeCatalogSource) Set(revision string, r FakeRevision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revisions[revision] = r
}

// Loads returns the revisions Load was called with, in order.
func (f *FakeCatalogSource) Loads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.loads)
}

// Head implements CatalogSource.
func (f *FakeCatalogSource) Head(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, f.headErr
}

// Load implements CatalogSource; an unknown revision is an error.
func (f *FakeCatalogSource) Load(ctx context.Context, revision string) (*catalog.Catalog, []catalog.Error, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, revision)
	r, ok := f.revisions[revision]
	if !ok {
		return nil, nil, fmt.Errorf("fake source: unknown revision %q", revision)
	}
	return r.Catalog, slices.Clone(r.Diagnostics), r.Err
}

func (f *FakeCatalogSource) String() string { return "fake" }
