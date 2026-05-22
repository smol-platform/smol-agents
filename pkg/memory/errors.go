package memory

import "errors"

// Kind classifies a memory error so the internal transport can round-trip it
// and the gateway can map it to an MCP error + audit reason. Fail-closed:
// an unknown/zero kind is treated as Internal. Implements R-MEM-SEC-1.
type Kind string

const (
	KindUnauthenticated    Kind = "unauthenticated"     // no/invalid SVID
	KindPermissionDenied   Kind = "permission_denied"   // policy or tenant denial
	KindQuotaExceeded      Kind = "quota_exceeded"      // topK/rate/size limit
	KindNotFound           Kind = "not_found"           // unknown id/namespace/branch
	KindNotSupported       Kind = "not_supported"       // op not implemented by backend
	KindInvalid            Kind = "invalid"             // malformed request
	KindBackendUnavailable Kind = "backend_unavailable" // backend/transport failure
	KindInternal           Kind = "internal"            // unclassified
)

// Error is a typed memory error carrying a Kind. The gateway, workers, and the
// internal transport all use it so a denial/quota/not-found classification
// survives the gateway↔worker hop and the MCP boundary.
type Error struct {
	Kind Kind
	Msg  string
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return string(e.Kind)
	}
	return string(e.Kind) + ": " + e.Msg
}

// Typed constructors (use these instead of fmt.Errorf so the Kind is set).
func Unauthenticated(msg string) *Error    { return &Error{KindUnauthenticated, msg} }
func PermissionDenied(msg string) *Error   { return &Error{KindPermissionDenied, msg} }
func QuotaExceeded(msg string) *Error      { return &Error{KindQuotaExceeded, msg} }
func NotFound(msg string) *Error           { return &Error{KindNotFound, msg} }
func Invalid(msg string) *Error            { return &Error{KindInvalid, msg} }
func BackendUnavailable(msg string) *Error { return &Error{KindBackendUnavailable, msg} }
func Internal(msg string) *Error           { return &Error{KindInternal, msg} }

// KindOf extracts the Kind from any error. An *Error returns its Kind; an
// *ErrNotSupported maps to KindNotSupported; anything else (incl. nil-wrapped)
// is KindInternal. Returns "" only for a nil error.
func KindOf(err error) Kind {
	if err == nil {
		return ""
	}
	var me *Error
	if errors.As(err, &me) {
		return me.Kind
	}
	var ns *ErrNotSupported
	if errors.As(err, &ns) {
		return KindNotSupported
	}
	return KindInternal
}
