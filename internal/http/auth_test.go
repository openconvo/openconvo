package http

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
)

// testAdminPassword is what every test in this package signs in with.
const testAdminPassword = "correct horse battery staple"

func newTestAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	a, err := NewAuthenticator(testAdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func loginCookie(t *testing.T, handler http.Handler, password string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
		strings.NewReader(`{"password":`+strconv.Quote(password)+`}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status = %d: %s", rec.Code, rec.Body.String())
	}
	for _, cookie := range rec.Result().Cookies() {
		if cookie.Name == sessionCookieName {
			return cookie
		}
	}
	t.Fatal("login did not set a session cookie")
	return nil
}

func TestNewAuthenticatorRejectsShortPassword(t *testing.T) {
	if _, err := NewAuthenticator("too-short"); err == nil {
		t.Fatal("NewAuthenticator accepted a short password")
	}
}

func TestAuthenticationProtectsAPIAndLeavesHealthPublic(t *testing.T) {
	a := newTestAuthenticator(t)
	handler := newTestHandler(Deps{
		Auth:          a,
		CheckDatabase: func(context.Context) error { return nil },
		Status:        func(context.Context) (StatusResponse, error) { return StatusResponse{}, nil },
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("health = %d, want 200", rec.Code)
	}

	for _, path := range []string{"/api/v1/system/status", "/api/v1/system/update", "/api/v1/search?q=private-message", "/api/v1/backups", "/api/v1/backups/0198c0de-0000-4000-8000-00000000bacc/content"} {
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d, want 401", path, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("unauthenticated %s Cache-Control = %q", path, rec.Header().Get("Cache-Control"))
		}
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/auth/session", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Errorf("session status = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPRouteUsesItsIndependentAuthenticationBoundary(t *testing.T) {
	called := 0
	mcpHandler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called++
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	handler := New(Config{Addr: ":0"}, Deps{
		Logger: testLogger(), Auth: newTestAuthenticator(t), MCP: mcpHandler,
	}).http.Handler

	// No browser session is present. The real MCP handler performs its own
	// bearer check; the HTTP router must not interpose cookie authentication.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusNoContent || called != 1 {
		t.Fatalf("MCP route = %d, calls %d", rec.Code, called)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("browser API without session = %d, want 401", rec.Code)
	}
}

func TestDisabledMCPRouteReturns404InsteadOfSPA(t *testing.T) {
	handler := New(Config{Addr: ":0"}, Deps{
		Logger: testLogger(), Auth: newTestAuthenticator(t), WebAssets: spaAssets(),
	}).http.Handler
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("disabled MCP route = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("disabled MCP Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// A Deps without an authenticator used to publish the archive: New skipped
// Protect entirely and every route answered anyone. It must fail closed
// instead — nothing about a half-assembled server makes serving private
// conversations to unidentified callers the safer option.
func TestAPIRefusesToServeWithoutAnAuthenticator(t *testing.T) {
	fake := newFakeArchive()
	fake.attachments[testAttachmentUUID] = archive.StoredAttachment{
		ID: testAttachmentUUID, Filename: "carnival.webp", Size: 4, SHA256: strings.Repeat("5", 64),
	}
	handler := New(Config{Addr: ":0"}, Deps{
		Logger:  testLogger(),
		Archive: fake,
		Status:  func(context.Context) (StatusResponse, error) { return StatusResponse{}, nil },
	}).http.Handler

	for _, path := range []string{
		"/api/v1/system/status",
		"/api/v1/channels/" + testUUID + "/messages",
		"/api/v1/messages/" + testMessageUUID,
		"/api/v1/attachments/" + testAttachmentUUID + "/content",
		"/api/v1/auth/session",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without an authenticator = %d, want 503 (%s)", path, rec.Code, rec.Body.String())
		}
	}

	// The public liveness route and the frontend still work: an operator has
	// to be able to see that the server is up and misconfigured.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("health without an authenticator = %d, want 200", rec.Code)
	}
}

func TestLoginSessionAndLogout(t *testing.T) {
	a := newTestAuthenticator(t)
	handler := newTestHandler(Deps{
		Auth:   a,
		Status: func(context.Context) (StatusResponse, error) { return StatusResponse{}, nil },
	})

	bad := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session", strings.NewReader(`{"password":"wrong password"}`))
	bad.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, bad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login = %d, want 401", rec.Code)
	}

	cookie := loginCookie(t, handler, testAdminPassword)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge <= 0 {
		t.Errorf("session cookie flags = %+v", cookie)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated API = %d: %s", rec.Code, rec.Body.String())
	}

	logout := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	logout.Header.Set("Origin", "http://example.com")
	logout.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, logout)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d", rec.Code)
	}
	cleared := rec.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Errorf("logout cookie = %+v", cleared)
	}

	// Clearing the browser's copy is not logging out: the token stays valid
	// for its full 12 hours, and anyone who kept a copy can replay it.
	replay := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	replay.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, replay)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("replayed logged-out session = %d, want 401", rec.Code)
	}

	// Logging in again must still work, and must not be caught by the
	// revocation of the previous session.
	fresh := loginCookie(t, handler, testAdminPassword)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
	req.AddCookie(fresh)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("session issued after logout = %d: %s", rec.Code, rec.Body.String())
	}
}

// A logged-out token is remembered only until it would have expired anyway,
// and only when it verified in the first place — a forged cookie must not be
// able to grow the set.
func TestRevokedSessionsAreForgottenAndNotForgeable(t *testing.T) {
	a := newTestAuthenticator(t)
	baseTime := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return baseTime }
	handler := newTestHandler(Deps{Auth: a})
	cookie := loginCookie(t, handler, testAdminPassword)

	forged := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	forged.Header.Set("Origin", "http://example.com")
	forged.AddCookie(&http.Cookie{Name: sessionCookieName, Value: cookie.Value + "x"})
	handler.ServeHTTP(httptest.NewRecorder(), forged)
	if len(a.revoked) != 0 {
		t.Errorf("forged logout recorded %d revocations", len(a.revoked))
	}

	logout := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	logout.Header.Set("Origin", "http://example.com")
	logout.AddCookie(cookie)
	handler.ServeHTTP(httptest.NewRecorder(), logout)
	if len(a.revoked) != 1 {
		t.Fatalf("revoked = %v, want the logged-out session", a.revoked)
	}

	// Past the session's own expiry the entry is dead weight: the next
	// logout drops it.
	a.now = func() time.Time { return baseTime.Add(sessionLifetime + time.Minute) }
	later := loginCookie(t, handler, testAdminPassword)
	logout = httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
	logout.Header.Set("Origin", "http://example.com")
	logout.AddCookie(later)
	handler.ServeHTTP(httptest.NewRecorder(), logout)
	if len(a.revoked) != 1 {
		t.Errorf("revoked = %v, want only the still-live session", a.revoked)
	}
}

// POST and DELETE /api/v1/auth/session are the only state-changing routes
// outside Protect — logging in cannot require being logged in — so each
// carries its own same-origin check. Nothing else tests those two checks: the
// Protect-level one is a different code path.
func TestSessionRoutesRejectCrossOriginRequests(t *testing.T) {
	for _, tc := range []struct {
		name   string
		origin string
		set    bool
	}{
		// A form POST from another site arrives with that site's Origin.
		{"another host, same scheme", "http://attacker.example", true},
		{"another host over https", "https://attacker.example", true},
		// Our host as a prefix of theirs must not read as ours.
		{"our host as their subdomain", "http://example.com.attacker.example", true},
		// Some cross-site requests send no Origin at all.
		{"no Origin header", "", false},
		{"empty Origin header", "", true},
	} {
		a := newTestAuthenticator(t)
		handler := newTestHandler(Deps{
			Auth:   a,
			Status: func(context.Context) (StatusResponse, error) { return StatusResponse{}, nil },
		})
		cookie := loginCookie(t, handler, testAdminPassword)

		login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
			strings.NewReader(`{"password":"`+testAdminPassword+`"}`))
		if tc.set {
			login.Header.Set("Origin", tc.origin)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, login)
		if rec.Code != http.StatusForbidden {
			t.Errorf("login with %s = %d, want 403 (%s)", tc.name, rec.Code, rec.Body.String())
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("login with %s issued a session anyway", tc.name)
		}

		logout := httptest.NewRequest(http.MethodDelete, "/api/v1/auth/session", nil)
		if tc.set {
			logout.Header.Set("Origin", tc.origin)
		}
		logout.AddCookie(cookie)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, logout)
		if rec.Code != http.StatusForbidden {
			t.Errorf("logout with %s = %d, want 403", tc.name, rec.Code)
		}

		// A rejected logout must not have ended the session either.
		req := httptest.NewRequest(http.MethodGet, "/api/v1/system/status", nil)
		req.AddCookie(cookie)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("session after a rejected logout with %s = %d", tc.name, rec.Code)
		}
	}
}

// The counterpart: the frontend's own requests, which is what proves the
// check above rejects for the right reason rather than rejecting everything.
func TestSessionRoutesAcceptSameOriginRequests(t *testing.T) {
	a := newTestAuthenticator(t)
	handler := newTestHandler(Deps{Auth: a})

	for _, host := range []string{"example.com", "archive.example.com:8443"} {
		login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
			strings.NewReader(`{"password":"`+testAdminPassword+`"}`))
		login.Host = host
		// Behind a TLS-terminating proxy the browser's Origin says https
		// while the request itself arrives over plain HTTP. The host is
		// what has to match.
		login.Header.Set("Origin", "https://"+host)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, login)
		if rec.Code != http.StatusOK {
			t.Errorf("same-origin login as %s = %d (%s)", host, rec.Code, rec.Body.String())
		}
	}
}

func TestSessionTamperingExpiryAndCSRFRejected(t *testing.T) {
	a := newTestAuthenticator(t)
	baseTime := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return baseTime }
	handler := newTestHandler(Deps{Auth: a})
	cookie := loginCookie(t, handler, testAdminPassword)

	tampered := *cookie
	tampered.Value += "x"
	req := httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	req.AddCookie(&tampered)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered session = %d, want 401", rec.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/v1/unknown", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("missing Origin = %d, want 403", rec.Code)
	}

	// Both schemes, because the host is what must reject these: a check
	// that only compared schemes would pass the first of them.
	for _, origin := range []string{"https://attacker.example", "http://attacker.example"} {
		req = httptest.NewRequest(http.MethodPost, "/api/v1/unknown", nil)
		req.Header.Set("Origin", origin)
		req.AddCookie(cookie)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("cross-origin request from %s = %d, want 403", origin, rec.Code)
		}
	}

	a.now = func() time.Time { return baseTime.Add(sessionLifetime + time.Second) }
	req = httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired session = %d, want 401", rec.Code)
	}
}

func TestSecureCookieBehindHTTPSProxy(t *testing.T) {
	a := newTestAuthenticator(t)
	handler := newTestHandler(Deps{Auth: a})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
		strings.NewReader(`{"password":"`+testAdminPassword+`"}`))
	req.Header.Set("Origin", "https://archive.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Host = "archive.example.com"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d: %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure {
		t.Errorf("HTTPS proxy session cookie = %+v, want Secure", cookies)
	}
}

func TestFailedLoginsAreRateLimited(t *testing.T) {
	a := newTestAuthenticator(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	handler := newTestHandler(Deps{Auth: a})

	for attempt := 0; attempt < maxLoginFailures; attempt++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
			strings.NewReader(`{"password":"definitely incorrect"}`))
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", attempt+1, rec.Code)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
		strings.NewReader(`{"password":"`+testAdminPassword+`"}`))
	req.Header.Set("Origin", "http://example.com")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("limited login = %d headers=%v", rec.Code, rec.Header())
	}

	now = now.Add(loginWindow)
	cookie := loginCookie(t, handler, testAdminPassword)
	if cookie == nil {
		t.Fatal("login did not recover after rate-limit window")
	}
}

// OpenConvo has no trusted-proxy allowlist, so a direct client must not be able
// to rotate a claimed forwarding address to escape the rate limit.
func TestFailedLoginsIgnoreUntrustedForwardedFor(t *testing.T) {
	a := newTestAuthenticator(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	handler := newTestHandler(Deps{Auth: a})

	failLogin := func(forwardedFor string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/session",
			strings.NewReader(`{"password":"definitely incorrect"}`))
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("X-Forwarded-For", forwardedFor)
		req.RemoteAddr = "203.0.113.7:44000"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	for attempt := 0; attempt < maxLoginFailures; attempt++ {
		claimed := fmt.Sprintf("198.51.100.%d", attempt+1)
		if code := failLogin(claimed); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", attempt+1, code)
		}
	}
	if code := failLogin("10.0.0.1"); code != http.StatusTooManyRequests {
		t.Errorf("forged forwarding address escaped the limit: %d", code)
	}
}

// The failed-login table is keyed by an address a caller can influence, so it
// must have a ceiling rather than grow for as long as failures arrive.
func TestFailedLoginTableIsBounded(t *testing.T) {
	a := newTestAuthenticator(t)
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	for i := 0; i < maxLoginKeys*2; i++ {
		a.recordLoginFailure("203.0.113." + strconv.Itoa(i))
	}
	if len(a.loginAttempts) > maxLoginKeys {
		t.Fatalf("login attempts = %d entries, want at most %d", len(a.loginAttempts), maxLoginKeys)
	}
	// The most recent caller is the one still being counted.
	if _, tracked := a.loginAttempts["203.0.113."+strconv.Itoa(maxLoginKeys*2-1)]; !tracked {
		t.Error("the newest failure was evicted instead of the oldest")
	}
}
