package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/embeddings"
	"github.com/openconvo/openconvo/internal/updates"
	"github.com/openconvo/openconvo/internal/version"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestHandler builds the real server, gate included. Deps.Auth is required
// there — a nil one refuses every API route — so a test that is not about
// authentication still gets an authenticator, and the handler it returns signs
// each request in first: the same session cookie and same-origin Origin header
// a browser carries. Tests that supply their own Auth get the bare handler and
// manage sessions themselves.
func newTestHandler(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = testLogger()
	}
	if deps.Auth != nil {
		return New(Config{Addr: ":0"}, deps).http.Handler
	}
	auth, err := NewAuthenticator(testAdminPassword)
	if err != nil {
		panic(err)
	}
	deps.Auth = auth
	return signedIn{handler: New(Config{Addr: ":0"}, deps).http.Handler, auth: auth}
}

type signedIn struct {
	handler http.Handler
	auth    *Authenticator
}

func (s signedIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(sessionCookieName); err != nil {
		issued := httptest.NewRecorder()
		if err := s.auth.issueSession(issued, r); err != nil {
			panic(err)
		}
		for _, cookie := range issued.Result().Cookies() {
			r.AddCookie(cookie)
		}
	}
	if r.Header.Get("Origin") == "" {
		r.Header.Set("Origin", "http://"+r.Host)
	}
	s.handler.ServeHTTP(w, r)
}

func TestHealthOK(t *testing.T) {
	handler := newTestHandler(Deps{
		CheckDatabase: func(context.Context) error { return nil },
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Checks["database"] != "ok" {
		t.Errorf("body = %+v", body)
	}
	if rec.Header().Get("X-Request-ID") == "" {
		t.Error("X-Request-ID header missing")
	}
}

func TestHealthDegradedWhenDatabaseDown(t *testing.T) {
	// A pgx dial failure names the host, port, database and user. /health is
	// public, so the document may say the database is down and nothing more.
	dsnDetail := `failed to connect to host=db.internal user=openconvo database=openconvo: connection refused`
	handler := newTestHandler(Deps{
		CheckDatabase: func(context.Context) error { return errors.New(dsnDetail) },
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "degraded") {
		t.Errorf("body = %s", rec.Body.String())
	}
	for _, leaked := range []string{"db.internal", "openconvo", "connection refused"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Errorf("public health document leaked %q: %s", leaked, rec.Body.String())
		}
	}
}

func TestStatusEndpoint(t *testing.T) {
	want := StatusResponse{
		Version:   version.Get(),
		StartedAt: time.Now().UTC(),
		Database:  DatabaseStatus{Connected: true, SchemaVersion: 1},
		Storage:   StorageStatus{Driver: "filesystem", Path: "/data/attachments"},
		Discord: DiscordStatus{
			Configured: true, Connected: true, ApplicationID: "123",
			BotUsername: "openconvo", LastError: "",
		},
		Counts: &CountsStatus{Messages: 42},
	}
	handler := newTestHandler(Deps{
		Status: func(context.Context) (StatusResponse, error) { return want, nil },
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Database.Connected || got.Database.SchemaVersion != 1 {
		t.Errorf("database = %+v", got.Database)
	}
	if got.Counts == nil || got.Counts.Messages != 42 {
		t.Errorf("counts = %+v", got.Counts)
	}
	if got.Storage.Driver != "filesystem" {
		t.Errorf("storage = %+v", got.Storage)
	}
	if !got.Discord.Configured || !got.Discord.Connected ||
		got.Discord.ApplicationID != "123" || got.Discord.BotUsername != "openconvo" {
		t.Errorf("discord = %+v", got.Discord)
	}
}

func TestStatusEndpointError(t *testing.T) {
	handler := newTestHandler(Deps{
		Status: func(context.Context) (StatusResponse, error) {
			return StatusResponse{}, errors.New("db exploded")
		},
	})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	// The error detail must not leak to clients.
	if strings.Contains(rec.Body.String(), "exploded") {
		t.Error("internal error detail leaked in response")
	}
}

// Every other dependency answers 503 when it was not wired; Status used to be
// the one that panicked into a 500 instead.
func TestStatusEndpointWithoutDependency(t *testing.T) {
	handler := newTestHandler(Deps{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

type fakeUpdateChecker struct {
	status updates.Status
	err    error
}

func (f fakeUpdateChecker) Check(context.Context) (updates.Status, error) {
	return f.status, f.err
}

func TestUpdateEndpoint(t *testing.T) {
	want := updates.Status{
		CurrentVersion:        "1.2.0",
		LatestVersion:         "1.3.0",
		UpdateAvailable:       true,
		CommandUpgradeAllowed: true,
		Reason:                "update-available",
		ReleaseURL:            "https://github.com/openconvo/openconvo/releases/tag/v1.3.0",
		UpgradeCommand:        "docker compose pull openconvo",
	}
	handler := newTestHandler(Deps{Updates: fakeUpdateChecker{status: want}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/update", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var got updates.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.CommandUpgradeAllowed || got.LatestVersion != want.LatestVersion || got.UpgradeCommand == "" {
		t.Fatalf("body = %+v", got)
	}
	if gotCache := rec.Header().Get("Cache-Control"); gotCache != "private, no-store" {
		t.Errorf("Cache-Control = %q", gotCache)
	}
}

func TestUpdateEndpointDoesNotLeakUpstreamError(t *testing.T) {
	handler := newTestHandler(Deps{Updates: fakeUpdateChecker{err: errors.New("token=secret")}})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/update", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("internal error leaked: %s", rec.Body.String())
	}
}

func TestUnknownAPIRouteReturnsJSON(t *testing.T) {
	handler := newTestHandler(Deps{})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %s, want JSON", ct)
	}
}

func TestPanicRecovery(t *testing.T) {
	deps := Deps{
		Logger: testLogger(),
		Status: func(context.Context) (StatusResponse, error) { panic("boom") },
	}
	handler := newTestHandler(deps)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 after panic", rec.Code)
	}
}

// The backup and attachment downloads replace the server's WriteTimeout with
// a deadline of their own. That only works if http.NewResponseController can
// reach the connection through the middleware chain; when it cannot it returns
// http.ErrNotSupported and the long download is cut off at WriteTimeout with
// nothing in the log to say why.
func TestWriteDeadlineReachesTheConnection(t *testing.T) {
	deadlineErr := make(chan error, 1)
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineErr <- http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Minute))
		w.WriteHeader(http.StatusNoContent)
	})
	logger := testLogger()
	server := httptest.NewServer(requestID(logging(logger, recoverer(logger, probe))))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := <-deadlineErr; err != nil {
		t.Fatalf("SetWriteDeadline through the middleware chain: %v", err)
	}
}

func spaAssets() fs.FS {
	return fstest.MapFS{
		"index.html":            {Data: []byte("<html>openconvo shell</html>")},
		"assets/app-abc123.js":  {Data: []byte("console.log('app')")},
		"assets/app-abc123.css": {Data: []byte("body{}")},
		"favicon.svg":           {Data: []byte("<svg/>")},
	}
}

func TestSPAServesIndexAndAssets(t *testing.T) {
	handler := newTestHandler(Deps{WebAssets: spaAssets()})

	for _, tc := range []struct {
		path         string
		wantContains string
		wantCache    string
	}{
		{"/", "openconvo shell", "no-cache"},
		{"/assets/app-abc123.js", "console.log", "immutable"},
		// Client-side routes fall back to the shell.
		{"/channels/some-uuid", "openconvo shell", "no-cache"},
		{"/search?q=veneer", "openconvo shell", "no-cache"},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d", tc.path, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), tc.wantContains) {
			t.Errorf("%s: body = %q", tc.path, rec.Body.String())
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), tc.wantCache) {
			t.Errorf("%s: Cache-Control = %q, want %q", tc.path, rec.Header().Get("Cache-Control"), tc.wantCache)
		}
	}
}

// SPAHandler is given the raw request path, so this exercises it directly:
// through the mux, ServeMux cleans a literal ".." segment and answers 301
// before any handler sees it, which tests the standard library rather than
// this package.
func TestSPAPathTraversalBlocked(t *testing.T) {
	handler := SPAHandler(spaAssets())
	for _, tc := range []struct {
		name, path string
		wantCode   int
		wantBody   string
		wantCache  string
	}{
		// A dot segment never reaches a filesystem: net/http refuses the
		// request outright, and path.Clean would have flattened it before
		// the lookup anyway.
		{"escape attempt", "/../../etc/passwd", http.StatusBadRequest, "invalid URL path", ""},
		{"escape and return", "/assets/../../etc/passwd", http.StatusBadRequest, "invalid URL path", ""},
		// Naming a host path without dot segments is just an unknown
		// client-side route: the shell answers, no host file is reachable.
		{"host path", "/etc/passwd", http.StatusOK, "openconvo shell", "no-cache"},
		// And the cleaning SPAHandler does itself is real: an odd but safe
		// path still resolves to the file it names, rather than falling
		// through to the shell as an unreadable name would.
		{"redundant separators", "/assets//app-abc123.js", http.StatusOK, "console.log", "immutable"},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = tc.path
		handler.ServeHTTP(rec, req)

		if rec.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.name, rec.Code, tc.wantCode)
		}
		if !strings.Contains(rec.Body.String(), tc.wantBody) {
			t.Errorf("%s: body = %q, want it to contain %q", tc.name, rec.Body.String(), tc.wantBody)
		}
		if strings.Contains(rec.Body.String(), "root:") {
			t.Errorf("%s: served a file from outside the embedded assets", tc.name)
		}
		if tc.wantCache != "" && !strings.Contains(rec.Header().Get("Cache-Control"), tc.wantCache) {
			t.Errorf("%s: Cache-Control = %q, want %q", tc.name, rec.Header().Get("Cache-Control"), tc.wantCache)
		}
	}
}

func TestSPAFallbackPageWithoutBuild(t *testing.T) {
	handler := newTestHandler(Deps{WebAssets: nil})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "frontend assets are not embedded") {
		t.Errorf("fallback page missing: %s", rec.Body.String())
	}
}

func TestSPARejectsWriteMethods(t *testing.T) {
	handler := newTestHandler(Deps{WebAssets: spaAssets()})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

type fakeArchive struct {
	communities []archive.Community
	channels    map[string][]archive.Channel
	byID        map[string]archive.Channel
	archiveRows []archive.ArchiveChannel
	enabled     map[string]bool
	overview    []archive.SyncOverviewRow
	messagePage archive.MessagePage
	contexts    map[string]archive.MessageContext
	searchPage  archive.SearchPage
	searches    []archive.SearchParams
	attachments map[string]archive.StoredAttachment
	// attachmentLookups records every ID that reached the store, so a test
	// can tell "not found" apart from "never asked".
	attachmentLookups []string
	bookmarks         []archive.Bookmark
}

func (f *fakeArchive) ListCommunities(context.Context) ([]archive.Community, error) {
	return f.communities, nil
}

func (f *fakeArchive) ListChannels(_ context.Context, communityID string) ([]archive.Channel, error) {
	return f.channels[communityID], nil
}

func (f *fakeArchive) ListArchiveChannels(context.Context) ([]archive.ArchiveChannel, error) {
	return f.archiveRows, nil
}

func (f *fakeArchive) GetChannel(_ context.Context, id string) (archive.Channel, bool, error) {
	ch, ok := f.byID[id]
	return ch, ok, nil
}

func (f *fakeArchive) GetArchiveChannel(_ context.Context, id string) (archive.ArchiveChannel, bool, error) {
	for _, ch := range f.archiveRows {
		if ch.ID == id {
			return ch, true, nil
		}
	}
	return archive.ArchiveChannel{}, false, nil
}

func (f *fakeArchive) SetChannelArchiveEnabled(_ context.Context, id string, enabled bool) error {
	if _, ok := f.byID[id]; !ok {
		return archive.ErrNotFound
	}
	f.enabled[id] = enabled
	ch := f.byID[id]
	ch.ArchiveEnabled = enabled
	f.byID[id] = ch
	return nil
}

func (f *fakeArchive) SyncOverview(context.Context) ([]archive.SyncOverviewRow, error) {
	return f.overview, nil
}

func (f *fakeArchive) ListMessages(_ context.Context, channelID, before string, limit int) (archive.MessagePage, error) {
	return f.messagePage, nil
}

func (f *fakeArchive) GetMessageContext(_ context.Context, id string, before, after int) (archive.MessageContext, bool, error) {
	context, ok := f.contexts[id]
	return context, ok, nil
}

func (f *fakeArchive) SearchMessages(_ context.Context, params archive.SearchParams) (archive.SearchPage, error) {
	f.searches = append(f.searches, params)
	return f.searchPage, nil
}

type fakeSemanticSearch struct {
	page     archive.SearchPage
	searches []archive.SearchParams
	err      error
}

func (f *fakeSemanticSearch) SearchMessages(_ context.Context, params archive.SearchParams) (archive.SearchPage, error) {
	f.searches = append(f.searches, params)
	return f.page, f.err
}

func (f *fakeArchive) GetStoredAttachment(_ context.Context, id string) (archive.StoredAttachment, bool, error) {
	f.attachmentLookups = append(f.attachmentLookups, id)
	attachment, ok := f.attachments[id]
	return attachment, ok, nil
}

func (f *fakeArchive) ListBookmarks(_ context.Context, filter archive.BookmarkFilter) ([]archive.Bookmark, error) {
	out := []archive.Bookmark{}
	for _, bookmark := range f.bookmarks {
		if filter.Collection != "" && bookmark.Collection != filter.Collection {
			continue
		}
		if filter.Tag != "" {
			found := false
			for _, tag := range bookmark.Tags {
				found = found || tag == filter.Tag
			}
			if !found {
				continue
			}
		}
		out = append(out, bookmark)
	}
	return out, nil
}

func (f *fakeArchive) CreateBookmark(_ context.Context, in archive.BookmarkUpsert) (archive.Bookmark, bool, error) {
	for _, bookmark := range f.bookmarks {
		if bookmark.MessageID == in.MessageID {
			return bookmark, false, nil
		}
	}
	bookmark := archive.Bookmark{
		ID: testBookmarkUUID, MessageID: in.MessageID, Title: in.Title,
		Description: in.Description, Tags: in.Tags, Collection: in.Collection,
	}
	f.bookmarks = append(f.bookmarks, bookmark)
	return bookmark, true, nil
}

func (f *fakeArchive) UpdateBookmark(_ context.Context, id string, in archive.BookmarkUpsert) (archive.Bookmark, error) {
	for i := range f.bookmarks {
		if f.bookmarks[i].ID == id {
			f.bookmarks[i].Title = in.Title
			f.bookmarks[i].Description = in.Description
			f.bookmarks[i].Tags = in.Tags
			f.bookmarks[i].Collection = in.Collection
			return f.bookmarks[i], nil
		}
	}
	return archive.Bookmark{}, archive.ErrNotFound
}

func (f *fakeArchive) DeleteBookmark(_ context.Context, id string) error {
	for i := range f.bookmarks {
		if f.bookmarks[i].ID == id {
			f.bookmarks = append(f.bookmarks[:i], f.bookmarks[i+1:]...)
			return nil
		}
	}
	return archive.ErrNotFound
}

const testUUID = "0198c0de-0000-4000-8000-000000000001"
const testCommunityUUID = "0198c0de-0000-4000-8000-0000000000c0"
const testMessageUUID = "0198c0de-0000-4000-8000-0000000000aa"
const testBookmarkUUID = "0198c0de-0000-4000-8000-0000000000bb"

func newFakeArchive() *fakeArchive {
	ch := archive.Channel{ID: testUUID, CommunityID: testCommunityUUID, ExternalID: "c1", Kind: "text", Name: "deck-making"}
	return &fakeArchive{
		communities: []archive.Community{{ID: testCommunityUUID, Source: "discord", ExternalID: "g1", Name: "FBFR"}},
		channels:    map[string][]archive.Channel{testCommunityUUID: {ch}},
		byID:        map[string]archive.Channel{testUUID: ch},
		archiveRows: []archive.ArchiveChannel{{
			ID: testUUID, CommunityID: testCommunityUUID, CommunityName: "FBFR", Kind: "text", Name: "deck-making", MessageCount: 42,
		}},
		enabled:     map[string]bool{},
		attachments: map[string]archive.StoredAttachment{},
		contexts:    map[string]archive.MessageContext{},
		overview: []archive.SyncOverviewRow{
			{ChannelID: testUUID, ChannelName: "deck-making", CommunityName: "FBFR", Kind: "text", Status: "importing", MessageCount: 42},
		},
	}
}

func TestListCommunitiesAndChannels(t *testing.T) {
	handler := newTestHandler(Deps{Archive: newFakeArchive()})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/communities", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "FBFR") {
		t.Fatalf("communities: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/communities/"+testCommunityUUID+"/channels", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "deck-making") {
		t.Fatalf("channels: %d %s", rec.Code, rec.Body.String())
	}

	// A malformed ID is answered here, not by the database as a 500.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/communities/not-a-uuid/channels", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed community ID: %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestToggleChannelArchive(t *testing.T) {
	fake := newFakeArchive()
	var toggled []string
	handler := newTestHandler(Deps{
		Archive: fake,
		OnChannelToggle: func(_ context.Context, id string, enabled bool) error {
			toggled = append(toggled, fmt.Sprintf("%s=%v", id, enabled))
			return nil
		},
	})

	body := strings.NewReader(`{"enabled": true}`)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/channels/"+testUUID+"/archive", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: %d %s", rec.Code, rec.Body.String())
	}
	if !fake.enabled[testUUID] {
		t.Error("channel not enabled in store")
	}
	if len(toggled) != 1 || toggled[0] != testUUID+"=true" {
		t.Errorf("OnChannelToggle calls = %v", toggled)
	}
	var resp struct {
		Channel archive.Channel `json:"channel"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || !resp.Channel.ArchiveEnabled {
		t.Errorf("response = %s", rec.Body.String())
	}
}

// Real guilds put channels inside categories, which makes the category
// their parent. Only threads follow a parent; a categorized text channel
// must stay selectable.
func TestToggleAcceptsChannelInsideACategory(t *testing.T) {
	fake := newFakeArchive()
	category := "0198c0de-0000-4000-8000-0000000000ca"
	nested := "0198c0de-0000-4000-8000-0000000000be"
	fake.byID[category] = archive.Channel{ID: category, Kind: "category", Name: "Text Channels"}
	fake.byID[nested] = archive.Channel{
		ID: nested, Kind: "text", Name: "general", ParentChannelID: &category,
	}
	handler := newTestHandler(Deps{Archive: fake})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
		"/api/v1/channels/"+nested+"/archive", strings.NewReader(`{"enabled":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("channel inside a category: %d %s", rec.Code, rec.Body.String())
	}
	if !fake.enabled[nested] {
		t.Error("channel inside a category was not enabled")
	}
}

func TestToggleRejectsBadInput(t *testing.T) {
	fake := newFakeArchive()
	// A thread (which has a parent) and a voice channel are not selectable.
	parent := testUUID
	fake.byID["0198c0de-0000-4000-8000-000000000002"] = archive.Channel{
		ID: "0198c0de-0000-4000-8000-000000000002", Kind: "thread", ParentChannelID: &parent,
	}
	fake.byID["0198c0de-0000-4000-8000-000000000003"] = archive.Channel{
		ID: "0198c0de-0000-4000-8000-000000000003", Kind: "voice",
	}
	fake.byID["0198c0de-0000-4000-8000-000000000004"] = archive.Channel{
		ID: "0198c0de-0000-4000-8000-000000000004", Kind: "category",
	}
	handler := newTestHandler(Deps{Archive: fake})

	cases := []struct {
		path, body string
		want       int
	}{
		{"/api/v1/channels/" + testUUID + "/archive", `not json`, http.StatusBadRequest},
		{"/api/v1/channels/0198c0de-0000-4000-8000-000000000002/archive", `{"enabled":true}`, http.StatusBadRequest}, // thread
		{"/api/v1/channels/0198c0de-0000-4000-8000-000000000003/archive", `{"enabled":true}`, http.StatusBadRequest}, // voice
		{"/api/v1/channels/0198c0de-0000-4000-8000-000000000004/archive", `{"enabled":true}`, http.StatusBadRequest}, // category
		{"/api/v1/channels/0198c0de-0000-4000-8000-00000000dead/archive", `{"enabled":true}`, http.StatusNotFound},   // unknown
		{"/api/v1/channels/not-a-uuid/archive", `{"enabled":true}`, http.StatusNotFound},                             // malformed id
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("PUT %s: %d, want %d (%s)", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestSyncOverviewEndpoint(t *testing.T) {
	handler := newTestHandler(Deps{Archive: newFakeArchive()})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d", rec.Code)
	}
	var resp struct {
		Channels []archive.SyncOverviewRow `json:"channels"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Channels) != 1 || resp.Channels[0].MessageCount != 42 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestArchiveChannelAndMessageEndpoints(t *testing.T) {
	fake := newFakeArchive()
	content := "Titebond III works well"
	message := archive.ArchiveMessage{
		ID: testMessageUUID, ChannelID: testUUID, Content: &content,
		SourceCreatedAt: time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Actor:           &archive.ArchiveActor{ID: "actor-1", DisplayName: "Alex"},
		Attachments:     []archive.ArchiveAttachment{},
		Reactions:       []archive.Reaction{},
	}
	fake.messagePage = archive.MessagePage{Messages: []archive.ArchiveMessage{message}, HasOlder: true}
	fake.contexts[testMessageUUID] = archive.MessageContext{
		Channel: fake.archiveRows[0], TargetID: testMessageUUID,
		Messages: []archive.ArchiveMessage{message},
	}
	handler := newTestHandler(Deps{Archive: fake})

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/api/v1/channels", "deck-making"},
		{"/api/v1/channels/" + testUUID + "/messages?limit=25", "Titebond III"},
		{"/api/v1/messages/" + testMessageUUID, `"target_id"`},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("GET %s: %d %s", tc.path, rec.Code, rec.Body.String())
		}
		if cache := rec.Header().Get("Cache-Control"); cache != "private, no-store" {
			t.Errorf("GET %s: Cache-Control = %q", tc.path, cache)
		}
	}
}

func TestArchiveMessageEndpointValidation(t *testing.T) {
	handler := newTestHandler(Deps{Archive: newFakeArchive()})
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/v1/channels/not-a-uuid/messages", http.StatusNotFound},
		{"/api/v1/channels/" + testUUID + "/messages?before=nope", http.StatusBadRequest},
		{"/api/v1/channels/" + testUUID + "/messages?limit=101", http.StatusBadRequest},
		{"/api/v1/messages/not-a-uuid", http.StatusNotFound},
		{"/api/v1/messages/" + testMessageUUID + "?before=51", http.StatusBadRequest},
		{"/api/v1/messages/" + testMessageUUID, http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s: %d, want %d (%s)", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestSearchEndpoint(t *testing.T) {
	fake := newFakeArchive()
	fake.searchPage = archive.SearchPage{
		Results: []archive.SearchResult{{
			MessageID: testMessageUUID, ChannelID: testUUID,
			ChannelName: "deck-making", CommunityName: "FBFR",
			Excerpt: "Use <mark>maple</mark> veneer", HasAttachment: true,
			SourceCreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		}},
		HasMore: true,
	}
	handler := newTestHandler(Deps{Archive: fake})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=maple+veneer&channel_id="+testUUID+
			"&author=John&after=2026-01-01&before=2026-02-01&has_attachment=true&limit=25&offset=50", nil))
	var response archive.SearchPage
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || len(response.Results) != 1 || response.Results[0].Excerpt != "Use <mark>maple</mark> veneer" {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "private, no-store" {
		t.Errorf("Cache-Control = %q", cache)
	}
	if len(fake.searches) != 1 {
		t.Fatalf("search calls = %d", len(fake.searches))
	}
	got := fake.searches[0]
	if got.Query != "maple veneer" || got.ChannelID != testUUID || got.Author != "John" ||
		got.After == nil || got.Before == nil || got.HasAttachment == nil || !*got.HasAttachment ||
		got.Limit != 25 || got.Offset != 50 {
		t.Errorf("search params = %+v", got)
	}
}

func TestSemanticSearchEndpoint(t *testing.T) {
	archiveFake := newFakeArchive()
	semantic := &fakeSemanticSearch{page: archive.SearchPage{
		Results: []archive.SearchResult{{
			MessageID: testMessageUUID, ChannelID: testUUID,
			ChannelName: "deck-making", CommunityName: "FBFR",
			Excerpt:         "Use hide glue for this joint.",
			SourceCreatedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		}},
	}}
	handler := newTestHandler(Deps{Archive: archiveFake, SemanticSearch: semantic})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/search?q=bonding+wood&mode=semantic&channel_id="+testUUID+"&limit=25", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Use hide glue") {
		t.Fatalf("semantic search: %d %s", rec.Code, rec.Body.String())
	}
	if len(semantic.searches) != 1 || semantic.searches[0].Query != "bonding wood" ||
		semantic.searches[0].ChannelID != testUUID || semantic.searches[0].Limit != 25 {
		t.Fatalf("semantic search params = %+v", semantic.searches)
	}
	if len(archiveFake.searches) != 0 {
		t.Fatalf("semantic mode called keyword search: %+v", archiveFake.searches)
	}
}

func TestSemanticSearchEndpointErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{embeddings.ErrDisabled, http.StatusConflict},
		{embeddings.ErrNotReady, http.StatusConflict},
		{embeddings.ErrNotConfigured, http.StatusServiceUnavailable},
		{embeddings.ErrProvider, http.StatusBadGateway},
	} {
		handler := newTestHandler(Deps{
			Archive:        newFakeArchive(),
			SemanticSearch: &fakeSemanticSearch{err: tc.err},
		})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/api/v1/search?q=query&mode=semantic", nil))
		if rec.Code != tc.want {
			t.Errorf("error %v: status %d, want %d (%s)", tc.err, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestSearchEndpointValidation(t *testing.T) {
	handler := newTestHandler(Deps{Archive: newFakeArchive()})
	for _, path := range []string{
		"/api/v1/search",
		"/api/v1/search?q=ok&channel_id=nope",
		"/api/v1/search?q=ok&after=yesterday",
		"/api/v1/search?q=ok&after=2026-02-01&before=2026-01-01",
		"/api/v1/search?q=ok&has_attachment=maybe",
		"/api/v1/search?q=ok&limit=101",
		"/api/v1/search?q=ok&offset=-1",
		"/api/v1/search?q=ok&mode=hybrid",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s: %d, want 400 (%s)", path, rec.Code, rec.Body.String())
		}
	}
}

func TestBookmarkEndpoints(t *testing.T) {
	fake := newFakeArchive()
	handler := newTestHandler(Deps{Archive: fake})

	createBody := `{
		"message_id":"` + testMessageUUID + `",
		"title":" Glue recommendation ",
		"description":"Tested advice",
		"tags":[" Glue ","glue","Veneer"],
		"collection":" Deck making "
	}`
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/bookmarks", strings.NewReader(createBody)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if len(fake.bookmarks) != 1 || fake.bookmarks[0].Title != "Glue recommendation" ||
		fake.bookmarks[0].Collection != "Deck making" || fmt.Sprint(fake.bookmarks[0].Tags) != "[glue veneer]" {
		t.Fatalf("normalized bookmark = %+v", fake.bookmarks)
	}
	if cache := rec.Header().Get("Cache-Control"); cache != "private, no-store" {
		t.Errorf("create Cache-Control = %q", cache)
	}

	// Saving twice is an idempotent 200 and keeps the same row.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/bookmarks", strings.NewReader(createBody)))
	if rec.Code != http.StatusOK || len(fake.bookmarks) != 1 {
		t.Fatalf("duplicate create: %d rows=%d", rec.Code, len(fake.bookmarks))
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/bookmarks?collection=Deck+making&tag=glue", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Glue recommendation") {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/v1/bookmarks/"+testBookmarkUUID,
		strings.NewReader(`{"title":"Pressing pressure","description":"","tags":["press"],"collection":"Deck making"}`)))
	if rec.Code != http.StatusOK || fake.bookmarks[0].Title != "Pressing pressure" {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/bookmarks/"+testBookmarkUUID, nil))
	if rec.Code != http.StatusNoContent || len(fake.bookmarks) != 0 {
		t.Fatalf("delete: %d rows=%d", rec.Code, len(fake.bookmarks))
	}
}

func TestBookmarkEndpointValidation(t *testing.T) {
	handler := newTestHandler(Deps{Archive: newFakeArchive()})
	cases := []struct {
		method string
		path   string
		body   string
		want   int
	}{
		{http.MethodPost, "/api/v1/bookmarks", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/bookmarks", `{"message_id":"` + testMessageUUID + `","title":"","description":"","tags":[],"collection":"","unknown":true}`, http.StatusBadRequest},
		{http.MethodPost, "/api/v1/bookmarks", `{"message_id":"` + testMessageUUID + `","title":"","description":"","tags":["` + strings.Repeat("x", 51) + `"],"collection":""}`, http.StatusBadRequest},
		{http.MethodPut, "/api/v1/bookmarks/not-a-uuid", `{}`, http.StatusNotFound},
		// An update cannot move a bookmark to another message, and must say
		// so rather than accept message_id and drop it.
		{http.MethodPut, "/api/v1/bookmarks/" + testBookmarkUUID,
			`{"message_id":"` + testMessageUUID + `","title":"","description":"","tags":[],"collection":""}`,
			http.StatusBadRequest},
		{http.MethodDelete, "/api/v1/bookmarks/not-a-uuid", ``, http.StatusNotFound},
		{http.MethodDelete, "/api/v1/bookmarks/0198c0de-0000-4000-8000-00000000dead", ``, http.StatusNotFound},
	}
	for _, tc := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s %s: %d, want %d (%s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
}

// routeRegistration matches the two muxes New builds its routing table on.
var routeRegistration = regexp.MustCompile(`\b(mux|api)\.Handle(?:Func)?\("([^"]+)"`)

// registeredRoutes reads the routing table out of server.go. A list kept by
// hand here would go stale the moment someone adds a route, which is the
// mistake these tests exist to catch.
func registeredRoutes(t *testing.T) (outer, protected []string) {
	t.Helper()
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	matches := routeRegistration.FindAllStringSubmatch(string(source), -1)
	if len(matches) < 20 {
		t.Fatalf("only %d route registrations found in server.go; this test no longer sees the routing table", len(matches))
	}
	for _, match := range matches {
		if match[1] == "mux" {
			outer = append(outer, match[2])
			continue
		}
		protected = append(protected, match[2])
	}
	return outer, protected
}

// requestFor turns a ServeMux pattern into a request that reaches it.
func requestFor(pattern string) *http.Request {
	method, path := http.MethodGet, pattern
	if verb, rest, found := strings.Cut(pattern, " "); found {
		method, path = verb, rest
	}
	if path == "/" {
		// The api mux's catch-all, which answers unknown API routes.
		path = "/api/v1/does-not-exist"
	}
	path = strings.ReplaceAll(path, "{id}", testUUID)
	return httptest.NewRequest(method, path, nil)
}

// Authentication is one Protect around the whole /api/ subtree, and three
// tests prove that gate fires — but Go's ServeMux gives a literal pattern
// precedence over a subtree pattern, so a route registered on the outer mux
// bypasses Protect entirely, and three already are for reasons that do not
// generalise. This walks the real routing table instead of a list: a new route
// that escapes the gate fails here on the day it is added.
func TestEveryAPIRouteRequiresASession(t *testing.T) {
	outer, protected := registeredRoutes(t)

	// The only patterns that may sit outside Protect, each because it cannot
	// require a session: container liveness, the independently bearer-protected
	// MCP endpoint, the three session routes (signing in cannot require being
	// signed in), the gate itself, and the frontend.
	public := map[string]bool{
		"GET /health":                 true,
		"/mcp":                        true,
		"GET /api/v1/auth/session":    true,
		"POST /api/v1/auth/session":   true,
		"DELETE /api/v1/auth/session": true,
		"/api/":                       true,
		"/":                           true,
	}
	for _, pattern := range outer {
		if !public[pattern] {
			t.Errorf("%q is registered on the outer mux, so Protect never runs for it; register it on api instead", pattern)
		}
	}
	// The two routes that hand back message bodies, named so a regexp that
	// stops matching cannot quietly empty this test.
	for _, want := range []string{"GET /api/v1/channels/{id}/messages", "GET /api/v1/messages/{id}"} {
		if !slices.Contains(protected, want) {
			t.Errorf("%q is no longer among the routes read from server.go", want)
		}
	}

	// Every dependency is wired, so a route that escaped the gate answers
	// with real data rather than a 503 that could be mistaken for a refusal.
	fake := newFakeArchive()
	fake.attachments[testUUID] = archive.StoredAttachment{ID: testUUID, Filename: "a.bin", Size: 1, SHA256: strings.Repeat("5", 64)}
	handler := newTestHandler(Deps{
		Auth:           newTestAuthenticator(t),
		Archive:        fake,
		Blobs:          &memoryBlobStore{objects: map[string][]byte{strings.Repeat("5", 64): []byte("x")}},
		Backups:        &fakeBackups{},
		Embeddings:     &fakeEmbeddingSettings{},
		SemanticSearch: &fakeSemanticSearch{},
		Updates:        fakeUpdateChecker{},
		CheckDatabase:  func(context.Context) error { return nil },
		Status:         func(context.Context) (StatusResponse, error) { return StatusResponse{}, nil },
	})
	for _, pattern := range protected {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, requestFor(pattern))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %q = %d, want 401 (%s)", pattern, rec.Code, rec.Body.String())
		}
		if cache := rec.Header().Get("Cache-Control"); cache != "no-store" {
			t.Errorf("unauthenticated %q Cache-Control = %q, want no-store", pattern, cache)
		}
	}
}

func TestSelectionRoutesWithoutArchiveDep(t *testing.T) {
	handler := newTestHandler(Deps{}) // Archive nil
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/communities", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("communities without archive dep: %d, want 503", rec.Code)
	}
}
