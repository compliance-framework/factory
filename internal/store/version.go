package store

import "strconv"

// FormatVersion renders a resourceVersion: the store's global monotonic counter, in decimal.
func FormatVersion(v uint64) string { return strconv.FormatUint(v, 10) }

// ParseVersion parses a resourceVersion. ErrInvalid when it isn't one.
func ParseVersion(op string, k Key, s string) (uint64, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, Errorf(op, k, ErrInvalid, "malformed resourceVersion %q", s)
	}
	return v, nil
}
