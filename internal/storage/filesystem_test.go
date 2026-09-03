package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Filesystem {
	t.Helper()
	fs, err := NewFilesystem(filepath.Join(t.TempDir(), "attachments"))
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	return fs
}

func TestPutOpenRoundtrip(t *testing.T) {
	fs := newTestStore(t)
	ctx := context.Background()
	content := []byte("maple veneer, 0.6mm, from the supplier in Lyon")

	res, err := fs.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	wantDigest := sha256.Sum256(content)
	if res.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("SHA256 = %s, want %s", res.SHA256, hex.EncodeToString(wantDigest[:]))
	}
	if res.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", res.Size, len(content))
	}
	if res.AlreadyExisted {
		t.Error("first Put reported AlreadyExisted")
	}
	if !strings.HasPrefix(res.ObjectKey, "sha256/"+res.SHA256[:2]+"/") {
		t.Errorf("ObjectKey = %q, want sharded layout", res.ObjectKey)
	}

	r, err := fs.Open(ctx, res.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: %q", got)
	}
}

func TestPutDeduplicates(t *testing.T) {
	fs := newTestStore(t)
	ctx := context.Background()
	content := []byte("same file uploaded twice")

	first, err := fs.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	second, err := fs.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 {
		t.Errorf("digests differ: %s vs %s", first.SHA256, second.SHA256)
	}
	if !second.AlreadyExisted {
		t.Error("second Put did not report AlreadyExisted")
	}

	// Only one physical file, and no leftover temp files.
	var files int
	err = filepath.Walk(filepath.Join(fs.Root(), "sha256"), func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 {
		t.Errorf("physical files = %d, want 1", files)
	}
	tmpEntries, err := os.ReadDir(filepath.Join(fs.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpEntries) != 0 {
		t.Errorf("leftover temp files: %d", len(tmpEntries))
	}
}

func TestOpenMissing(t *testing.T) {
	fs := newTestStore(t)
	digest := strings.Repeat("ab", 32)
	if _, err := fs.Open(context.Background(), digest); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open missing = %v, want ErrNotFound", err)
	}
	exists, err := fs.Exists(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("Exists = true for missing blob")
	}
}

func TestDelete(t *testing.T) {
	fs := newTestStore(t)
	ctx := context.Background()
	res, err := fs.Put(ctx, strings.NewReader("temporary"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs.Delete(ctx, res.SHA256); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	exists, err := fs.Exists(ctx, res.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("blob still exists after Delete")
	}
	// Deleting again is not an error.
	if err := fs.Delete(ctx, res.SHA256); err != nil {
		t.Errorf("second Delete: %v", err)
	}
}

func TestInvalidDigestsRejected(t *testing.T) {
	fs := newTestStore(t)
	ctx := context.Background()
	for _, bad := range []string{
		"",
		"short",
		strings.Repeat("g", 64),                // not hex
		strings.Repeat("AB", 32),               // uppercase
		"../../../../etc/passwd",               // traversal
		"..%2f..%2f" + strings.Repeat("a", 44), // encoded traversal
		strings.Repeat("a", 63) + "/",          // separator
	} {
		if _, err := fs.Open(ctx, bad); err == nil {
			t.Errorf("Open(%q) accepted invalid digest", bad)
		}
		if err := fs.Delete(ctx, bad); err == nil {
			t.Errorf("Delete(%q) accepted invalid digest", bad)
		}
		if _, err := fs.Exists(ctx, bad); err == nil {
			t.Errorf("Exists(%q) accepted invalid digest", bad)
		}
	}
}

func TestFilesystemWalkAndDeleteObjects(t *testing.T) {
	root := t.TempDir()
	store, err := NewFilesystem(root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Put(context.Background(), strings.NewReader("canonical"))
	if err != nil {
		t.Fatal(err)
	}
	tmp, err := os.CreateTemp(filepath.Join(root, "tmp"), "abandoned-")
	if err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}

	var canonical, temporary Object
	if err := store.WalkObjects(context.Background(), func(object Object) error {
		switch {
		case object.SHA256 == result.SHA256:
			canonical = object
		case object.Temporary:
			temporary = object
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if canonical.Key != result.ObjectKey || temporary.Key == "" {
		t.Fatalf("canonical=%+v temporary=%+v", canonical, temporary)
	}
	if err := store.DeleteObject(context.Background(), temporary); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteObject(context.Background(), Object{Key: "../outside", Temporary: true}); err == nil {
		t.Error("DeleteObject accepted path traversal")
	}
}

func TestLargeStreamingPut(t *testing.T) {
	fs := newTestStore(t)
	// 8 MiB of deterministic bytes via a reader, exercising the
	// streaming path without holding the content in one buffer.
	const size = 8 << 20
	res, err := fs.Put(context.Background(), io.LimitReader(deterministicReader{}, size))
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != size {
		t.Errorf("Size = %d, want %d", res.Size, size)
	}
}

type deterministicReader struct{}

func (deterministicReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i % 251)
	}
	return len(p), nil
}
