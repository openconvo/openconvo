// Package storage stores attachment files as content-addressed blobs.
//
// Blobs are identified by the SHA-256 of their content, which gives
// deduplication for free and lets exported archives be independently
// verified. The filesystem driver is the default; S3-compatible object
// storage is available for archives too large for the application host.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// ErrNotFound is returned when a blob does not exist in the store.
var ErrNotFound = errors.New("storage: blob not found")

// PutResult describes a stored blob.
type PutResult struct {
	// SHA256 is the lowercase hex digest of the blob content.
	SHA256 string
	// Size is the blob size in bytes.
	Size int64
	// ObjectKey is the driver-specific location, e.g.
	// "sha256/4a/4ad390ab...". Stored in the blobs table so exports can
	// reference physical files without knowing the driver.
	ObjectKey string
	// AlreadyExisted reports whether an identical blob was already stored.
	AlreadyExisted bool
}

// Store is a content-addressed blob store.
type Store interface {
	// Put streams content into the store, returning its digest and size.
	// Storing content that already exists is not an error; the existing
	// blob is reused.
	Put(ctx context.Context, r io.Reader) (PutResult, error)
	// Open returns a reader for a stored blob. The caller must close it.
	Open(ctx context.Context, sha256hex string) (io.ReadCloser, error)
	// Exists reports whether a blob is stored.
	Exists(ctx context.Context, sha256hex string) (bool, error)
	// Delete removes a blob. Deleting a missing blob is not an error.
	Delete(ctx context.Context, sha256hex string) error
}

// Object describes one physical item discovered while walking a store.
// SHA256 is set only for canonical content-addressed objects. Temporary
// objects are staging files which may safely be removed after they become
// stale; unknown objects are reported but never removed automatically.
type Object struct {
	Key       string
	SHA256    string
	Size      int64
	Modified  time.Time
	Temporary bool
}

// ObjectWalker is the optional maintenance surface implemented by the
// built-in storage drivers. It is deliberately separate from Store so narrow
// read-only test doubles and future serving-only drivers need not implement
// administrative enumeration.
type ObjectWalker interface {
	WalkObjects(ctx context.Context, visit func(Object) error) error
	DeleteObject(ctx context.Context, object Object) error
}

var sha256HexPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidateSHA256 rejects anything that is not a lowercase 64-character
// hex digest. Every driver must validate digests before using them in
// paths or keys: this is the path-traversal guard.
func ValidateSHA256(sha256hex string) error {
	if !sha256HexPattern.MatchString(sha256hex) {
		return fmt.Errorf("storage: invalid sha256 digest %q", sha256hex)
	}
	return nil
}

// ObjectKey returns the canonical object key for a digest: the first
// two hex characters shard the namespace so directories stay small.
func ObjectKey(sha256hex string) string {
	return "sha256/" + sha256hex[:2] + "/" + sha256hex
}
