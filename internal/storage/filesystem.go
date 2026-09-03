package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Filesystem stores blobs under a root directory:
//
//	<root>/sha256/4a/4ad390ab...
//	<root>/tmp/            (in-flight uploads)
//
// Writes stream through a temporary file while hashing, then rename
// atomically into place, so a crash mid-download never leaves a partial
// blob at a final path.
type Filesystem struct {
	root string
}

// NewFilesystem creates the storage root if needed and verifies it is
// writable.
func NewFilesystem(root string) (*Filesystem, error) {
	if root == "" {
		return nil, fmt.Errorf("storage: empty filesystem root")
	}
	for _, dir := range []string{root, filepath.Join(root, "tmp"), filepath.Join(root, "sha256")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("storage: create %s: %w", dir, err)
		}
	}
	probe, err := os.CreateTemp(filepath.Join(root, "tmp"), "probe-*")
	if err != nil {
		return nil, fmt.Errorf("storage: root %s is not writable: %w", root, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return &Filesystem{root: root}, nil
}

// Root returns the storage root directory.
func (f *Filesystem) Root() string { return f.root }

func (f *Filesystem) path(sha256hex string) string {
	return filepath.Join(f.root, "sha256", sha256hex[:2], sha256hex)
}

// Put implements Store. Content is never held in memory: it streams to
// a temporary file while the digest is computed.
func (f *Filesystem) Put(ctx context.Context, r io.Reader) (PutResult, error) {
	tmp, err := os.CreateTemp(filepath.Join(f.root, "tmp"), "put-*")
	if err != nil {
		return PutResult{}, fmt.Errorf("storage: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	hasher := sha256.New()
	size, err := io.Copy(io.MultiWriter(tmp, hasher), r)
	if err != nil {
		cleanup()
		return PutResult{}, fmt.Errorf("storage: write blob: %w", err)
	}
	if err := ctx.Err(); err != nil {
		cleanup()
		return PutResult{}, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return PutResult{}, fmt.Errorf("storage: sync blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return PutResult{}, fmt.Errorf("storage: close blob: %w", err)
	}

	digest := hex.EncodeToString(hasher.Sum(nil))
	result := PutResult{SHA256: digest, Size: size, ObjectKey: ObjectKey(digest)}
	final := f.path(digest)

	if _, err := os.Stat(final); err == nil {
		// Identical content already stored: deduplicate.
		_ = os.Remove(tmpName)
		result.AlreadyExisted = true
		return result, nil
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		_ = os.Remove(tmpName)
		return PutResult{}, fmt.Errorf("storage: create shard dir: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return PutResult{}, fmt.Errorf("storage: finalize blob: %w", err)
	}
	return result, nil
}

// Open implements Store.
func (f *Filesystem) Open(_ context.Context, sha256hex string) (io.ReadCloser, error) {
	if err := ValidateSHA256(sha256hex); err != nil {
		return nil, err
	}
	file, err := os.Open(f.path(sha256hex))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("storage: open blob %s: %w", sha256hex, err)
	}
	return file, nil
}

// Exists implements Store.
func (f *Filesystem) Exists(_ context.Context, sha256hex string) (bool, error) {
	if err := ValidateSHA256(sha256hex); err != nil {
		return false, err
	}
	_, err := os.Stat(f.path(sha256hex))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// Delete implements Store.
func (f *Filesystem) Delete(_ context.Context, sha256hex string) error {
	if err := ValidateSHA256(sha256hex); err != nil {
		return err
	}
	err := os.Remove(f.path(sha256hex))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// WalkObjects enumerates canonical blobs and staging files without following
// symlinks. Unexpected files are surfaced with no digest and are never
// eligible for automatic deletion.
func (f *Filesystem) WalkObjects(ctx context.Context, visit func(Object) error) error {
	for _, area := range []struct {
		dir       string
		temporary bool
	}{
		{dir: filepath.Join(f.root, "sha256")},
		{dir: filepath.Join(f.root, "tmp"), temporary: true},
	} {
		err := filepath.WalkDir(area.dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(f.root, path)
			if err != nil {
				return err
			}
			object := Object{
				Key:       filepath.ToSlash(rel),
				Size:      info.Size(),
				Modified:  info.ModTime(),
				Temporary: area.temporary,
			}
			if !area.temporary && info.Mode().IsRegular() {
				name := entry.Name()
				if ValidateSHA256(name) == nil && object.Key == ObjectKey(name) {
					object.SHA256 = name
				}
			}
			return visit(object)
		})
		if err != nil {
			return fmt.Errorf("storage: walk %s: %w", area.dir, err)
		}
	}
	return nil
}

// DeleteObject removes an item returned by WalkObjects. Re-validating its
// shape here prevents callers from turning the maintenance API into an
// arbitrary path deletion primitive.
func (f *Filesystem) DeleteObject(_ context.Context, object Object) error {
	var path string
	switch {
	case object.Temporary:
		if !strings.HasPrefix(object.Key, "tmp/") || filepath.Base(object.Key) != object.Key[len("tmp/"):] {
			return fmt.Errorf("storage: invalid temporary object key %q", object.Key)
		}
		path = filepath.Join(f.root, filepath.FromSlash(object.Key))
	case object.SHA256 != "":
		if err := ValidateSHA256(object.SHA256); err != nil || object.Key != ObjectKey(object.SHA256) {
			return fmt.Errorf("storage: invalid canonical object key %q", object.Key)
		}
		path = f.path(object.SHA256)
	default:
		return fmt.Errorf("storage: refusing to delete unknown object %q", object.Key)
	}
	err := os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
