package provider

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
)

// Registry maps Provider types to adapters. `factory serve` registers the GitHub adapter;
// tests register provider/fake under the type they stand in for. The zero value is ready to
// use and it is safe for concurrent use.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry returns a registry holding adapters.
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{}
	for _, a := range adapters {
		if err := r.Register(a); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Register adds a, keyed by a.Type(). A second adapter for a type is ErrAlreadyRegistered.
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return fmt.Errorf("register adapter: nil adapter: %w", ErrInvalid)
	}
	typ := a.Type()
	if typ == "" {
		return fmt.Errorf("register adapter: empty type: %w", ErrInvalid)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.adapters[typ]; ok {
		return fmt.Errorf("register adapter %q: %w", typ, ErrAlreadyRegistered)
	}
	if r.adapters == nil {
		r.adapters = map[string]Adapter{}
	}
	r.adapters[typ] = a
	return nil
}

// Lookup returns the adapter for typ.
func (r *Registry) Lookup(typ string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[typ]
	return a, ok
}

// Types returns the registered types, sorted.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.adapters))
	for t := range r.adapters {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// Connect connects p with the adapter for p.Spec.Type; ErrUnknownType when there is none.
func (r *Registry) Connect(ctx context.Context, p v1alpha1.Provider, secrets SecretResolver) (Conn, error) {
	a, ok := r.Lookup(string(p.Spec.Type))
	if !ok {
		return nil, fmt.Errorf("connect provider %q: type %q: %w", p.Metadata.Name, p.Spec.Type, ErrUnknownType)
	}
	c, err := a.Connect(ctx, p, secrets)
	if err != nil {
		return nil, fmt.Errorf("connect provider %q: %w", p.Metadata.Name, err)
	}
	return c, nil
}
