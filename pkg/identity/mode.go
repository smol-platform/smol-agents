package identity

import "fmt"

// Mode controls how strictly the platform enforces SPIFFE identity.
// Implements R-IDN-3.
type Mode string

const (
	ModeInsecure   Mode = "insecure"
	ModePermissive Mode = "permissive"
	ModeStrict     Mode = "strict"
	// ModeDegraded is set internally when the workload API is unreachable.
	// It is never user-selected.
	ModeDegraded Mode = "degraded"
)

func (m Mode) Valid() bool {
	switch m {
	case ModeInsecure, ModePermissive, ModeStrict, ModeDegraded:
		return true
	}
	return false
}

func (m Mode) String() string { return string(m) }

// ParseMode parses a user-supplied mode string. ModeDegraded is rejected
// because callers must not set it.
func ParseMode(s string) (Mode, error) {
	m := Mode(s)
	switch m {
	case ModeInsecure, ModePermissive, ModeStrict:
		return m, nil
	}
	return "", fmt.Errorf("identity: invalid mode %q", s)
}
