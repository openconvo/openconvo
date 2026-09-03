package http

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "openconvo_session"
	sessionLifetime   = 12 * time.Hour
	loginWindow       = 5 * time.Minute
	maxLoginFailures  = 10
	// maxLoginKeys bounds the failed-login table. Callers are identified by
	// their connection address, so the table remains bounded even under a
	// distributed scan.
	maxLoginKeys = 1024
)

type loginAttempt struct {
	first    time.Time
	failures int
}

// Authenticator implements the single-administrator authentication the
// archive is served behind. Passwords remain environment configuration; only a
// digest is retained here. Sessions are stateless, signed with a random key,
// and intentionally expire across process restarts.
//
// passwordDigest is a bare SHA-256, and deliberately so. A password KDF
// (argon2, bcrypt, pbkdf2) exists to slow down an attacker who has stolen a
// digest at rest; this digest is never at rest. It is derived once at startup
// from OPENCONVO_ADMIN_PASSWORD, which is already plaintext in the process
// environment, is never written to the database, a log line or a response, and
// is compared in constant time. An attacker who can read it can read the
// plaintext beside it, so stretching it would buy nothing. The one guarantee a
// KDF cannot replace is length, which NewAuthenticator enforces.
type Authenticator struct {
	passwordDigest [sha256.Size]byte
	sessionKey     [sha256.Size]byte
	now            func() time.Time
	loginMu        sync.Mutex
	loginAttempts  map[string]loginAttempt
	// revoked holds the nonces of sessions that logged out, until the
	// moment each would have expired anyway. Session cookies are
	// stateless, so this set is the only thing that makes logout mean
	// more than "the browser forgot".
	revokedMu sync.RWMutex
	revoked   map[string]time.Time
}

// NewAuthenticator constructs the administrator authenticator.
func NewAuthenticator(password string) (*Authenticator, error) {
	if len(password) < 12 {
		return nil, fmt.Errorf("admin password must be at least 12 characters")
	}
	a := &Authenticator{
		passwordDigest: sha256.Sum256([]byte(password)),
		now:            time.Now,
		loginAttempts:  make(map[string]loginAttempt),
		revoked:        make(map[string]time.Time),
	}
	if _, err := rand.Read(a.sessionKey[:]); err != nil {
		return nil, fmt.Errorf("generate session key: %w", err)
	}
	return a, nil
}

// Protect requires a valid administrator session and enforces same-origin
// requests for state-changing methods.
func (a *Authenticator) Protect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authenticated(r) {
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if isUnsafeMethod(r.Method) && !sameOrigin(r) {
			w.Header().Set("Cache-Control", "no-store")
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Authenticated reports whether the request carries a current, valid session.
func (a *Authenticator) Authenticated(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return false
	}
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || !a.now().Before(time.Unix(expires, 0)) {
		return false
	}
	if _, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil {
		return false
	}
	gotMAC, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	wantMAC := a.sign(parts[0] + "." + parts[1] + "." + parts[2])
	if !hmac.Equal(gotMAC, wantMAC) {
		return false
	}
	return !a.isRevoked(parts[2])
}

func (a *Authenticator) isRevoked(nonce string) bool {
	a.revokedMu.RLock()
	defer a.revokedMu.RUnlock()
	_, found := a.revoked[nonce]
	return found
}

// revokeSession stops the session the request carries from being accepted
// again. Only a session that verifies is recorded, so a forged cookie cannot
// grow the set, and every entry is dropped once the token it names would have
// expired on its own.
func (a *Authenticator) revokeSession(r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || !a.Authenticated(r) {
		return
	}
	parts := strings.Split(cookie.Value, ".")
	expires, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return
	}
	now := a.now()
	a.revokedMu.Lock()
	defer a.revokedMu.Unlock()
	for nonce, expiry := range a.revoked {
		if !now.Before(expiry) {
			delete(a.revoked, nonce)
		}
	}
	a.revoked[parts[2]] = time.Unix(expires, 0)
}

func (a *Authenticator) passwordMatches(password string) bool {
	digest := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(digest[:], a.passwordDigest[:]) == 1
}

func (a *Authenticator) issueSession(w http.ResponseWriter, r *http.Request) error {
	nonce := make([]byte, 18)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	expires := a.now().Add(sessionLifetime).UTC()
	payload := "v1." + strconv.FormatInt(expires.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	value := payload + "." + base64.RawURLEncoding.EncodeToString(a.sign(payload))
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(sessionLifetime / time.Second),
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
	return nil
}

func (a *Authenticator) clearSession(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		Expires:  time.Unix(1, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func (a *Authenticator) sign(payload string) []byte {
	mac := hmac.New(sha256.New, a.sessionKey[:])
	_, _ = mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func handleAuthStatus(a *Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": a.Authenticated(r)})
	}
}

func handleLogin(a *Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		key := loginKey(r)
		if allowed, retryAfter := a.loginAllowed(key); !allowed {
			w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Round(time.Second)/time.Second)))
			writeError(w, http.StatusTooManyRequests, "too many failed login attempts; try again later")
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || body.Password == "" {
			writeError(w, http.StatusBadRequest, `body must be {"password":"..."}`)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeError(w, http.StatusBadRequest, "body must contain one JSON object")
			return
		}
		if !a.passwordMatches(body.Password) {
			a.recordLoginFailure(key)
			writeError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		a.clearLoginFailures(key)
		if err := a.issueSession(w, r); err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create session")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
	}
}

func (a *Authenticator) loginAllowed(key string) (bool, time.Duration) {
	now := a.now()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	attempt := a.loginAttempts[key]
	if attempt.failures == 0 || !now.Before(attempt.first.Add(loginWindow)) {
		delete(a.loginAttempts, key)
		return true, 0
	}
	if attempt.failures < maxLoginFailures {
		return true, 0
	}
	return false, attempt.first.Add(loginWindow).Sub(now)
}

func (a *Authenticator) recordLoginFailure(key string) {
	now := a.now()
	a.loginMu.Lock()
	defer a.loginMu.Unlock()
	attempt, tracked := a.loginAttempts[key]
	if !tracked || !now.Before(attempt.first.Add(loginWindow)) {
		attempt = loginAttempt{first: now}
	}
	attempt.failures++
	if !tracked && len(a.loginAttempts) >= maxLoginKeys {
		a.makeLoginRoom(now)
	}
	a.loginAttempts[key] = attempt
}

// makeLoginRoom keeps the failed-login table at maxLoginKeys entries: it drops
// everything already outside its window and, when a flood of distinct callers
// leaves nothing expired to drop, the oldest entry. The scan costs one pass,
// and only when the table is full — never once per failed login.
func (a *Authenticator) makeLoginRoom(now time.Time) {
	oldestKey := ""
	var oldest time.Time
	for key, candidate := range a.loginAttempts {
		if !now.Before(candidate.first.Add(loginWindow)) {
			delete(a.loginAttempts, key)
			continue
		}
		if oldestKey == "" || candidate.first.Before(oldest) {
			oldestKey, oldest = key, candidate.first
		}
	}
	if len(a.loginAttempts) >= maxLoginKeys && oldestKey != "" {
		delete(a.loginAttempts, oldestKey)
	}
}

func (a *Authenticator) clearLoginFailures(key string) {
	a.loginMu.Lock()
	delete(a.loginAttempts, key)
	a.loginMu.Unlock()
}

// loginKey identifies the caller a failed login is counted against. Forwarding
// headers are intentionally ignored: OpenConvo has no trusted-proxy allowlist,
// so a directly connected attacker could forge them and evade the limit. A
// reverse proxy therefore shares one conservative failure budget. There is one
// administrator, making that safer than accepting an attacker-controlled key.
func loginKey(r *http.Request) string {
	return remoteHost(r.RemoteAddr)
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil && host != "" {
		return host
	}
	return remoteAddr
}

func handleLogout(a *Authenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request rejected")
			return
		}
		a.revokeSession(r)
		a.clearSession(w, r)
		w.WriteHeader(http.StatusNoContent)
	}
}

func isUnsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// sameOrigin reports whether the browser-supplied Origin names this server.
// The host is what decides it. The scheme a request arrived over is not
// knowable behind a TLS-terminating proxy without believing
// X-Forwarded-Proto, and a header the caller controls must never be able to
// decide whether a cross-site request passes — deriving the expected scheme
// from it meant a spoofed value rejected honest requests and, worse, that the
// host comparison was never the thing doing the work. Origin is set by the
// browser and a cross-site page cannot forge it, so comparing hosts is the
// check; the scheme is only sanity-checked to reject exotic origins.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// secureRequest reports whether the browser reached OpenConvo over HTTPS, so
// the session cookie can be marked Secure. This does trust X-Forwarded-Proto,
// which is how a self-hoster's TLS-terminating proxy reports it; a forged
// value only marks a cookie Secure that the forger's own browser will then
// refuse to send back over plain HTTP.
func secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0])
	return strings.EqualFold(forwarded, "https")
}
