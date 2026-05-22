package memory

import "github.com/google/uuid"

// newDocID returns a new random UUID string for use as a document ID.
// Shared by backend adapters that need to assign IDs when the caller does not
// provide one.
func newDocID() string { return uuid.NewString() }
