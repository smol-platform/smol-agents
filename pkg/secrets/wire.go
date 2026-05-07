package secrets

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// MaxFrameBytes is the maximum size of a single broker request/response.
// It bounds memory pressure from a hostile or buggy peer.
const MaxFrameBytes = 1 << 16 // 64 KiB

// requestKind enumerates broker requests on the wire.
type requestKind string

const (
	reqLease   requestKind = "lease"
	reqRefresh requestKind = "refresh"
	reqClose   requestKind = "close"
)

// request is the on-wire request shape.
type request struct {
	Kind  requestKind   `json:"kind"`
	Name  string        `json:"name,omitempty"`
	Lease string        `json:"lease,omitempty"` // for refresh
	TTL   time.Duration `json:"ttl,omitempty"`   // requested TTL (≤ MaxLeaseTTL)
}

// response is the on-wire response shape. ErrorMessage is non-empty on
// failure; on success Lease is populated.
type response struct {
	Lease        *Lease `json:"lease,omitempty"`
	ErrorCode    string `json:"error_code,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
}

// writeFrame encodes and writes a length-prefixed JSON frame.
func writeFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("secrets: encode: %w", err)
	}
	if len(body) > MaxFrameBytes {
		return fmt.Errorf("secrets: frame %d > max %d", len(body), MaxFrameBytes)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	return nil
}

// readFrame reads exactly one length-prefixed JSON frame and decodes it
// into v.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 {
		return errors.New("secrets: zero-length frame")
	}
	if n > MaxFrameBytes {
		return fmt.Errorf("secrets: frame %d > max %d", n, MaxFrameBytes)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// errorCodeFor maps known errors to stable wire codes.
func errorCodeFor(err error) string {
	switch {
	case errors.Is(err, ErrUnauthorized):
		return "unauthorized"
	case errors.Is(err, ErrNotFound):
		return "not_found"
	case errors.Is(err, ErrBackendDown):
		return "backend_down"
	case errors.Is(err, ErrPeerNotSpiffe):
		return "peer_not_spiffe"
	case errors.Is(err, ErrTTLExceeded):
		return "ttl_exceeded"
	case errors.Is(err, ErrLeaseExpired):
		return "lease_expired"
	case errors.Is(err, ErrInvalidRequest):
		return "invalid_request"
	default:
		return "internal"
	}
}

// errorFromCode maps a wire code back to a typed error.
func errorFromCode(code, msg string) error {
	base := fmt.Errorf("%s", msg)
	switch code {
	case "unauthorized":
		return errors.Join(ErrUnauthorized, base)
	case "not_found":
		return errors.Join(ErrNotFound, base)
	case "backend_down":
		return errors.Join(ErrBackendDown, base)
	case "peer_not_spiffe":
		return errors.Join(ErrPeerNotSpiffe, base)
	case "ttl_exceeded":
		return errors.Join(ErrTTLExceeded, base)
	case "lease_expired":
		return errors.Join(ErrLeaseExpired, base)
	case "invalid_request":
		return errors.Join(ErrInvalidRequest, base)
	default:
		return base
	}
}
