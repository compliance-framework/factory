package controllers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/compliance-framework/factory/internal/apis/v1alpha1"
	"github.com/compliance-framework/factory/internal/store"
)

// KindKillSwitch is a pause flag set by the pause command (internal kind, no API, no schema).
// A record exists while its scope is paused; resume deletes it.
const KindKillSwitch = "KillSwitch"

// KillSwitch is one pause flag: a scope (and, except for ScopeGlobal, a name) that is paused.
type KillSwitch struct {
	v1alpha1.TypeMeta
	Metadata v1alpha1.ObjectMeta `json:"metadata"`
	Scope    string              `json:"scope"`
	Target   string              `json:"target,omitzero"`
	// By is who paused it (an Actor id); At is when.
	By string    `json:"by,omitzero"`
	At time.Time `json:"at"`
}

var _ store.MetaObject = (*KillSwitch)(nil)

// GetName returns metadata.name.
func (k *KillSwitch) GetName() string { return k.Metadata.Name }

// GetObjectMeta implements store.MetaObject.
func (k *KillSwitch) GetObjectMeta() *v1alpha1.ObjectMeta { return &k.Metadata }

// KillSwitchName is the record name of scope/name: "global", or "<scope>:<name>".
func KillSwitchName(scope, name string) string {
	if scope == ScopeGlobal {
		return ScopeGlobal
	}
	return scope + ":" + name
}

// ValidKillSwitchScope reports whether scope is one of the pause scopes.
func ValidKillSwitchScope(scope string) bool {
	switch scope {
	case ScopeGlobal, ScopeProvider, ScopeQueue, ScopeWorkItem:
		return true
	}
	return false
}

// StoreKillSwitches is KillSwitches kept in the store: the pause/resume commands write the
// flags with Pause and Resume, and the rule controller's rails read them with Paused. It is the
// rule controller's default.
type StoreKillSwitches struct {
	Store store.Store
}

var _ KillSwitches = StoreKillSwitches{}

// Paused implements KillSwitches.
func (k StoreKillSwitches) Paused(ctx context.Context, scope, name string) (bool, error) {
	var rec KillSwitch
	err := k.Store.Get(ctx, store.Key{Kind: KindKillSwitch, Name: KillSwitchName(scope, name)}, &rec)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return false, nil
	default:
		return false, fmt.Errorf("read kill switch %s: %w", KillSwitchName(scope, name), err)
	}
}

// Pause sets the flag of scope/name (name is ignored for ScopeGlobal). Pausing a paused scope
// changes nothing.
func (k StoreKillSwitches) Pause(ctx context.Context, scope, name, by string, at time.Time) error {
	if !ValidKillSwitchScope(scope) {
		return fmt.Errorf("kill switch scope %q: %w", scope, store.ErrInvalid)
	}
	if scope == ScopeGlobal {
		name = ""
	} else if name == "" {
		return fmt.Errorf("kill switch %s: no name: %w", scope, store.ErrInvalid)
	}
	rec := &KillSwitch{
		TypeMeta: v1alpha1.TypeMeta{Kind: KindKillSwitch},
		Metadata: v1alpha1.ObjectMeta{Name: KillSwitchName(scope, name)},
		Scope:    scope, Target: name, By: by, At: at,
	}
	if err := k.Store.Create(ctx, rec); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("pause %s: %w", rec.Metadata.Name, err)
	}
	return nil
}

// Resume clears the flag of scope/name. Resuming a scope that isn't paused changes nothing.
func (k StoreKillSwitches) Resume(ctx context.Context, scope, name string) error {
	if !ValidKillSwitchScope(scope) {
		return fmt.Errorf("kill switch scope %q: %w", scope, store.ErrInvalid)
	}
	err := k.Store.Delete(ctx, store.Key{Kind: KindKillSwitch, Name: KillSwitchName(scope, name)})
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("resume %s: %w", KillSwitchName(scope, name), err)
	}
	return nil
}
