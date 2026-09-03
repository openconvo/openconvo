package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type testS3Server struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	objects  map[string][]byte
	putCount int
}

func newTestS3Server(t *testing.T) *testS3Server {
	t.Helper()
	fake := &testS3Server{t: t, objects: make(map[string][]byte)}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (f *testS3Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
		f.t.Errorf("%s %s was not signed: Authorization = %q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}
	if !strings.Contains(r.Header.Get("Authorization"), "/test-region/s3/aws4_request") {
		f.t.Errorf("%s %s was signed for the wrong region", r.Method, r.URL.Path)
	}

	const bucketPath = "/archive"
	if r.URL.Path == bucketPath && r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path == bucketPath && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		f.mu.Lock()
		type listedObject struct {
			Key  string `xml:"Key"`
			Size int    `xml:"Size"`
		}
		response := struct {
			XMLName     xml.Name       `xml:"ListBucketResult"`
			Contents    []listedObject `xml:"Contents"`
			IsTruncated bool           `xml:"IsTruncated"`
		}{IsTruncated: false}
		for key, body := range f.objects {
			response.Contents = append(response.Contents, listedObject{Key: key, Size: len(body)})
		}
		f.mu.Unlock()
		body, err := xml.Marshal(response)
		if err != nil {
			f.t.Errorf("marshal list response: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
		return
	}
	if !strings.HasPrefix(r.URL.Path, bucketPath+"/") {
		f.t.Errorf("request path = %q, want path-style bucket %q", r.URL.Path, bucketPath)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, bucketPath+"/")

	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodHead:
		body, ok := f.objects[key]
		if !ok {
			writeS3NotFound(w)
			return
		}
		w.Header().Set("Content-Length", stringInt64(int64(len(body))))
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			f.t.Errorf("read PUT body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.ContentLength != int64(len(body)) {
			f.t.Errorf("PUT content length = %d, body = %d", r.ContentLength, len(body))
		}
		f.objects[key] = body
		f.putCount++
		w.Header().Set("ETag", `"test"`)
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		body, ok := f.objects[key]
		if !ok {
			writeS3NotFound(w)
			return
		}
		w.Header().Set("Content-Length", stringInt64(int64(len(body))))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	case http.MethodDelete:
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func writeS3NotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
}

func stringInt64(v int64) string {
	// strconv.FormatInt spelled out here keeps response construction close to
	// the fake server without depending on fmt's broader formatting surface.
	return strconv.FormatInt(v, 10)
}

func newTestS3Store(t *testing.T) (*S3, *testS3Server) {
	t.Helper()
	fake := newTestS3Server(t)
	store, err := NewS3(context.Background(), S3Options{
		Endpoint:       fake.server.URL,
		Region:         "test-region",
		Bucket:         "archive",
		AccessKey:      "access-key",
		SecretKey:      "secret-key",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return store, fake
}

func TestS3PutOpenRoundtripAndDeduplicate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	store, fake := newTestS3Store(t)
	ctx := context.Background()
	content := []byte("an attachment that lives outside the OpenConvo host")

	first, err := store.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	wantDigest := sha256.Sum256(content)
	if first.SHA256 != hex.EncodeToString(wantDigest[:]) {
		t.Errorf("SHA256 = %s, want %s", first.SHA256, hex.EncodeToString(wantDigest[:]))
	}
	if first.Size != int64(len(content)) || first.ObjectKey != ObjectKey(first.SHA256) {
		t.Errorf("Put result = %+v", first)
	}
	if first.AlreadyExisted {
		t.Error("first Put reported AlreadyExisted")
	}

	second, err := store.Put(ctx, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("second Put: %v", err)
	}
	if !second.AlreadyExisted {
		t.Error("second Put did not report AlreadyExisted")
	}
	if fake.putCount != 1 {
		t.Errorf("PUT requests = %d, want 1", fake.putCount)
	}

	r, err := store.Open(ctx, first.SHA256)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content = %q, want %q", got, content)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("S3 staging files left behind: %v", entries)
	}
}

func TestS3MissingAndDeleteAreIdempotent(t *testing.T) {
	store, _ := newTestS3Store(t)
	ctx := context.Background()
	digest := strings.Repeat("ab", 32)

	if _, err := store.Open(ctx, digest); !errors.Is(err, ErrNotFound) {
		t.Errorf("Open missing = %v, want ErrNotFound", err)
	}
	exists, err := store.Exists(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("Exists = true for missing object")
	}
	if err := store.Delete(ctx, digest); err != nil {
		t.Errorf("Delete missing: %v", err)
	}
}

func TestS3Delete(t *testing.T) {
	store, _ := newTestS3Store(t)
	ctx := context.Background()
	res, err := store.Put(ctx, strings.NewReader("temporary remote object"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, res.SHA256); err != nil {
		t.Fatal(err)
	}
	exists, err := store.Exists(ctx, res.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("object still exists after Delete")
	}
}

func TestS3WalkAndDeleteObject(t *testing.T) {
	store, fake := newTestS3Store(t)
	ctx := context.Background()
	result, err := store.Put(ctx, strings.NewReader("walk me"))
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.objects["unexpected.txt"] = []byte("leave me alone")
	fake.mu.Unlock()

	var objects []Object
	if err := store.WalkObjects(ctx, func(object Object) error {
		objects = append(objects, object)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(objects) != 2 {
		t.Fatalf("objects = %+v, want two", objects)
	}
	var canonical, unknown Object
	for _, object := range objects {
		if object.SHA256 == result.SHA256 {
			canonical = object
		} else if object.Key == "unexpected.txt" {
			unknown = object
		}
	}
	if canonical.Key != result.ObjectKey || unknown.SHA256 != "" {
		t.Fatalf("walked objects = %+v", objects)
	}
	if err := store.DeleteObject(ctx, unknown); err == nil {
		t.Error("DeleteObject accepted unknown key")
	}
	if err := store.DeleteObject(ctx, canonical); err != nil {
		t.Fatal(err)
	}
}

func TestS3RefusesExistingObjectWithWrongSize(t *testing.T) {
	store, fake := newTestS3Store(t)
	content := []byte("canonical content")
	digest := sha256.Sum256(content)
	key := ObjectKey(hex.EncodeToString(digest[:]))
	fake.objects[key] = []byte("corrupt")

	if _, err := store.Put(context.Background(), bytes.NewReader(content)); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("Put over corrupt object = %v, want size mismatch", err)
	}
	if fake.putCount != 0 {
		t.Error("corrupt existing object was overwritten")
	}
}

func TestS3InvalidDigestsRejected(t *testing.T) {
	store, _ := newTestS3Store(t)
	ctx := context.Background()
	for _, bad := range []string{"", "short", strings.Repeat("g", 64), "../../etc/passwd"} {
		if _, err := store.Open(ctx, bad); err == nil {
			t.Errorf("Open(%q) accepted invalid digest", bad)
		}
		if _, err := store.Exists(ctx, bad); err == nil {
			t.Errorf("Exists(%q) accepted invalid digest", bad)
		}
		if err := store.Delete(ctx, bad); err == nil {
			t.Errorf("Delete(%q) accepted invalid digest", bad)
		}
	}
}

func TestNewS3ValidatesRequiredOptions(t *testing.T) {
	base := S3Options{Region: "region", Bucket: "bucket", AccessKey: "key", SecretKey: "secret"}
	cases := []S3Options{
		{Bucket: base.Bucket, AccessKey: base.AccessKey, SecretKey: base.SecretKey},
		{Region: base.Region, AccessKey: base.AccessKey, SecretKey: base.SecretKey},
		{Region: base.Region, Bucket: base.Bucket, SecretKey: base.SecretKey},
		{Region: base.Region, Bucket: base.Bucket, AccessKey: base.AccessKey},
	}
	for _, opts := range cases {
		if _, err := NewS3(context.Background(), opts); err == nil {
			t.Errorf("NewS3(%+v) accepted incomplete options", opts)
		}
	}
}
