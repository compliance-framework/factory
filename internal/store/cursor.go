package store

import (
	"encoding/base64"
	"encoding/json"
)

// cursor is a List position: the kind and the last name returned (lists are in name order, so
// a cursor stays valid while objects come and go).
type cursor struct {
	Kind string `json:"k"`
	Name string `json:"n"`
}

// EncodeCursor returns the opaque cursor that continues kind's list after name.
func EncodeCursor(kind, name string) string {
	b, _ := json.Marshal(cursor{Kind: kind, Name: name}) // two strings: cannot fail
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor returns the name after which kind's list continues ("" for an empty cursor).
// ErrInvalid for a malformed cursor or one from another kind's list.
func DecodeCursor(kind, s string) (string, error) {
	if s == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	var c cursor
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.Name == "" {
		return "", Errorf("list", Key{Kind: kind}, ErrInvalid, "malformed cursor")
	}
	if c.Kind != kind {
		return "", Errorf("list", Key{Kind: kind}, ErrInvalid, "cursor is for %s", c.Kind)
	}
	return c.Name, nil
}
