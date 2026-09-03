package http

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/storage"
)

type memoryBlobStore struct {
	objects map[string][]byte
	err     error
}

func (m *memoryBlobStore) Open(_ context.Context, digest string) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	body, ok := m.objects[digest]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(body)), nil
}

const testAttachmentUUID = "0198c0de-0000-4000-8000-0000000000a1"

func attachmentTestFixture(t *testing.T) (http.Handler, *http.Cookie, *fakeArchive) {
	t.Helper()
	a := newTestAuthenticator(t)
	fake := newFakeArchive()
	content := []byte("valid webp-shaped test bytes")
	digest := strings.Repeat("5", 64)
	fake.attachments[testAttachmentUUID] = archive.StoredAttachment{
		ID: testAttachmentUUID, Filename: "carnival.webp", ContentType: "image/webp",
		Size: int64(len(content)), SHA256: digest,
	}
	handler := newTestHandler(Deps{
		Auth:    a,
		Archive: fake,
		Blobs:   &memoryBlobStore{objects: map[string][]byte{digest: content}},
	})
	return handler, loginCookie(t, handler, testAdminPassword), fake
}

func attachmentTestHandler(t *testing.T) (http.Handler, *http.Cookie) {
	t.Helper()
	handler, cookie, _ := attachmentTestFixture(t)
	return handler, cookie
}

// getAttachment issues an authenticated request for an attachment ID exactly
// as given, so a test can send one the frontend would never produce.
func getAttachment(t *testing.T, handler http.Handler, cookie *http.Cookie, method, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/api/v1/attachments/"+id+"/content", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// Every other attachment test asks for a file that is served. These are the
// ways an attachment can be absent: never stored, deleted since, or an ID that
// is not an ID at all. All three must answer 404 and nothing else — a request
// the archive cannot satisfy must not become a 500, and must never reach the
// store as a value that was not a UUID.
func TestAttachmentContentNotFound(t *testing.T) {
	handler, cookie, fake := attachmentTestFixture(t)
	unknown := "0198c0de-0000-4000-8000-0000000000ff"

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := getAttachment(t, handler, cookie, method, unknown)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s unknown attachment = %d, want 404 (%s)", method, rec.Code, rec.Body.String())
		}
	}
	if !slices.Contains(fake.attachmentLookups, unknown) {
		t.Errorf("a well-formed unknown ID never reached the store: %v", fake.attachmentLookups)
	}

	// Deletion is honoured by the archive row, not by the blob: the bytes
	// stay in content-addressed storage until reclamation runs, and may be
	// shared with another attachment, so a deleted row must be the thing
	// that stops the download.
	delete(fake.attachments, testAttachmentUUID)
	rec := getAttachment(t, handler, cookie, http.MethodGet, testAttachmentUUID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("deleted attachment = %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "webp-shaped") {
		t.Error("deleted attachment still served its bytes")
	}

	// An ID that is not a UUID is refused here, before the store sees it.
	fake.attachmentLookups = nil
	for _, id := range []string{
		"..%2f..%2f..%2fetc%2fpasswd",
		"%2e%2e%2f%2e%2e%2fetc%2fpasswd",
		"carnival.webp",
		"0198c0de-0000-4000-8000-0000000000ff'%20or%20'1'='1",
	} {
		rec := getAttachment(t, handler, cookie, http.MethodGet, id)
		if rec.Code != http.StatusNotFound {
			t.Errorf("attachment ID %q = %d, want 404 (%s)", id, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("attachment ID %q served a file from outside the archive", id)
		}
	}
	if len(fake.attachmentLookups) != 0 {
		t.Errorf("malformed IDs reached the store: %v", fake.attachmentLookups)
	}
}

func TestAttachmentContentRequiresAuthentication(t *testing.T) {
	handler, _ := attachmentTestHandler(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/attachments/"+testAttachmentUUID+"/content", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAttachmentContentUsesCanonicalMetadata(t *testing.T) {
	handler, cookie := attachmentTestHandler(t)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/attachments/"+testAttachmentUUID+"/content", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "valid webp-shaped test bytes" {
		t.Errorf("body = %q", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/webp" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename=carnival.webp` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestAttachmentHeadAndMissingObject(t *testing.T) {
	handler, cookie := attachmentTestHandler(t)
	req := httptest.NewRequest(http.MethodHead,
		"/api/v1/attachments/"+testAttachmentUUID+"/content", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") == "" {
		t.Errorf("HEAD = %d headers=%v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}

	a := newTestAuthenticator(t)
	fake := newFakeArchive()
	digest := strings.Repeat("6", 64)
	fake.attachments[testAttachmentUUID] = archive.StoredAttachment{
		ID: testAttachmentUUID, Filename: "gone.bin", Size: 10, SHA256: digest,
	}
	missingHandler := newTestHandler(Deps{
		Auth: a, Archive: fake,
		Blobs: &memoryBlobStore{objects: map[string][]byte{}, err: storage.ErrNotFound},
	})
	missingCookie := loginCookie(t, missingHandler, testAdminPassword)
	req = httptest.NewRequest(http.MethodGet,
		"/api/v1/attachments/"+testAttachmentUUID+"/content", nil)
	req.AddCookie(missingCookie)
	rec = httptest.NewRecorder()
	missingHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "unavailable") {
		t.Errorf("missing object = %d %s", rec.Code, rec.Body.String())
	}
}

func TestSafeDownloadFilename(t *testing.T) {
	if got := safeDownloadFilename("../../secret\\name\n.webp"); strings.ContainsAny(got, "/\\\n") {
		t.Errorf("safeDownloadFilename = %q", got)
	}
	if got := safeDownloadFilename("\x00\n"); got != "attachment" {
		t.Errorf("empty safeDownloadFilename = %q", got)
	}
}

func TestAttachmentStorageFailureDoesNotLeakDetail(t *testing.T) {
	a := newTestAuthenticator(t)
	fake := newFakeArchive()
	fake.attachments[testAttachmentUUID] = archive.StoredAttachment{
		ID: testAttachmentUUID, Filename: "file.bin", Size: 1, SHA256: strings.Repeat("7", 64),
	}
	handler := newTestHandler(Deps{
		Auth: a, Archive: fake,
		Blobs: &memoryBlobStore{err: errors.New("secret endpoint detail")},
	})
	cookie := loginCookie(t, handler, testAdminPassword)
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/attachments/"+testAttachmentUUID+"/content", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || strings.Contains(rec.Body.String(), "secret endpoint") {
		t.Errorf("storage failure = %d %s", rec.Code, rec.Body.String())
	}
}
