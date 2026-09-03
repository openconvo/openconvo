// Package discordtest is an in-process fake of the Discord REST API and
// Gateway, so every sync test runs hermetically — OpenConvo's tests
// never talk to the real Discord. Behavior is a faithful subset of the
// real protocol: HELLO/IDENTIFY/RESUME/heartbeats on the gateway;
// pagination and fault injection on REST.
package discordtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// heartbeatIntervalMS is deliberately tiny so heartbeat and zombie
// behavior is testable in milliseconds rather than the real 41 seconds.
const heartbeatIntervalMS = 150

// sessionWaitTimeout bounds WaitForSession so a broken client fails the
// test instead of hanging it.
const sessionWaitTimeout = 10 * time.Second

// Server is a fake Discord: an httptest server serving the REST subset
// OpenConvo uses, plus a Gateway endpoint at /gateway.
type Server struct {
	ts *httptest.Server

	mu              sync.Mutex
	guilds          []guildFixture
	messages        map[string][]json.RawMessage // channel ID → newest-first
	activeThreads   map[string][]json.RawMessage // guild ID → threads
	archivedThreads map[string][]json.RawMessage // channel ID → threads
	requestCounts   map[string]int
	forbidden       map[string]bool
	messageHook     func()
	// failArmed injects one HTTP 500 into the message endpoint after
	// failAfter further successful requests.
	failArmed bool
	failAfter int

	identifies int
	resumes    int
	resumeSeq  int64
	dropACKs   bool
	sessionSeq int64
	// sessionsEstablished counts IDENTIFY/RESUME handshakes completed;
	// sessionsObserved counts those consumed by WaitForSession. Waiting
	// on the difference makes the helper independent of call ordering.
	sessionsEstablished int
	sessionsObserved    int
	// sessionCh is closed and replaced on every established session, so
	// waiters wake without polling.
	sessionCh chan struct{}

	conn *gwConn
}

type guildFixture struct {
	id, name string
	channels []json.RawMessage
}

type gwConn struct {
	ws      *websocket.Conn
	ctx     context.Context
	cancel  context.CancelFunc
	writeMu sync.Mutex
}

// New starts the fake server; it is closed automatically with the test.
func New(t *testing.T) *Server {
	s := &Server{
		messages:        map[string][]json.RawMessage{},
		activeThreads:   map[string][]json.RawMessage{},
		archivedThreads: map[string][]json.RawMessage{},
		requestCounts:   map[string]int{},
		forbidden:       map[string]bool{},
		sessionCh:       make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway", s.handleGateway)
	mux.HandleFunc("/", s.handleREST)
	s.ts = httptest.NewServer(mux)
	t.Cleanup(func() {
		s.mu.Lock()
		if s.conn != nil {
			s.conn.cancel()
		}
		s.mu.Unlock()
		s.ts.Close()
	})
	return s
}

// BaseURL is the REST base URL; pass it to Client.WithBaseURL.
func (s *Server) BaseURL() string { return s.ts.URL }

// GatewayURL is the WebSocket URL a client would connect to.
func (s *Server) GatewayURL() string { return s.wsBase() + "?v=10&encoding=json" }

func (s *Server) wsBase() string {
	return "ws" + strings.TrimPrefix(s.ts.URL, "http") + "/gateway"
}

// HeartbeatInterval is the interval the fake advertises in HELLO.
func (s *Server) HeartbeatInterval() time.Duration { return heartbeatIntervalMS * time.Millisecond }

// --- fixtures -------------------------------------------------------------

// AddGuild registers a guild and its channels.
func (s *Server) AddGuild(id, name string, channels ...json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.guilds = append(s.guilds, guildFixture{id: id, name: name, channels: channels})
}

// SetMessages sets a channel's history, newest first.
func (s *Server) SetMessages(channelID string, msgs []json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages[channelID] = msgs
}

// AddActiveThread registers an active thread of a guild.
func (s *Server) AddActiveThread(guildID string, thread json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activeThreads[guildID] = append(s.activeThreads[guildID], thread)
}

// AddArchivedThread registers an archived public thread of a channel.
func (s *Server) AddArchivedThread(channelID string, thread json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archivedThreads[channelID] = append(s.archivedThreads[channelID], thread)
}

// FailNextMessagesAfter arms one transient failure of the message
// endpoint: the next n requests succeed, the one after that returns
// HTTP 500, and later requests succeed again. n=0 fails the very next
// request.
func (s *Server) FailNextMessagesAfter(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failArmed = true
	s.failAfter = n
}

// ForbidChannel makes a channel's message endpoint return HTTP 403, as
// Discord does when the bot loses access.
func (s *Server) ForbidChannel(channelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forbidden[channelID] = true
}

// DropHeartbeatACKs simulates a zombie connection.
func (s *Server) DropHeartbeatACKs(drop bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropACKs = drop
}

// SetMessageResponseHook installs a test hook invoked after each messages
// response has been selected but before it is written. It lets integration
// tests model operator changes racing a multi-page synchronization.
func (s *Server) SetMessageResponseHook(hook func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageHook = hook
}

// Identifies reports how many IDENTIFY payloads were received.
func (s *Server) Identifies() int { s.mu.Lock(); defer s.mu.Unlock(); return s.identifies }

// Resumes reports how many RESUME payloads were received.
func (s *Server) Resumes() int { s.mu.Lock(); defer s.mu.Unlock(); return s.resumes }

// ResumeSequence reports the sequence supplied by the most recent RESUME.
func (s *Server) ResumeSequence() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resumeSeq
}

// RequestCount counts served requests whose path starts with prefix.
func (s *Server) RequestCount(pathPrefix string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p, c := range s.requestCounts {
		if strings.HasPrefix(p, pathPrefix) {
			n += c
		}
	}
	return n
}

// --- REST -----------------------------------------------------------------

func (s *Server) handleREST(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	s.mu.Lock()
	s.requestCounts[path]++
	s.mu.Unlock()

	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}

	switch {
	case path == "/gateway/bot":
		writeJSON(map[string]any{
			"url": s.wsBase(), "shards": 1,
			"session_start_limit": map[string]any{"total": 1000, "remaining": 999},
		})

	case path == "/users/@me":
		writeJSON(map[string]any{"id": "bot-user", "username": "openconvo", "bot": true})

	case path == "/users/@me/guilds":
		s.mu.Lock()
		out := []map[string]any{}
		for _, g := range s.guilds {
			out = append(out, map[string]any{"id": g.id, "name": g.name})
		}
		s.mu.Unlock()
		writeJSON(out)

	case strings.HasPrefix(path, "/guilds/") && strings.HasSuffix(path, "/channels"):
		guildID := strings.TrimSuffix(strings.TrimPrefix(path, "/guilds/"), "/channels")
		s.mu.Lock()
		var chans []json.RawMessage
		for _, g := range s.guilds {
			if g.id == guildID {
				chans = g.channels
			}
		}
		s.mu.Unlock()
		if chans == nil {
			chans = []json.RawMessage{}
		}
		writeJSON(chans)

	case strings.HasPrefix(path, "/guilds/") && strings.HasSuffix(path, "/threads/active"):
		guildID := strings.TrimSuffix(strings.TrimPrefix(path, "/guilds/"), "/threads/active")
		s.mu.Lock()
		threads := append([]json.RawMessage{}, s.activeThreads[guildID]...)
		s.mu.Unlock()
		writeJSON(map[string]any{"threads": threads, "members": []any{}})

	case strings.HasPrefix(path, "/guilds/"):
		guildID := strings.TrimPrefix(path, "/guilds/")
		s.mu.Lock()
		var found *guildFixture
		for i := range s.guilds {
			if s.guilds[i].id == guildID {
				found = &s.guilds[i]
			}
		}
		s.mu.Unlock()
		if found == nil {
			http.Error(w, `{"message":"Unknown Guild","code":10004}`, http.StatusNotFound)
			return
		}
		writeJSON(map[string]any{"id": found.id, "name": found.name})

	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/threads/archived/public"):
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "/channels/"), "/threads/archived/public")
		s.mu.Lock()
		threads := append([]json.RawMessage{}, s.archivedThreads[channelID]...)
		s.mu.Unlock()
		writeJSON(map[string]any{"threads": threads, "has_more": false})

	case strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/messages"):
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "/channels/"), "/messages")
		s.mu.Lock()
		if s.forbidden[channelID] {
			s.mu.Unlock()
			http.Error(w, `{"message":"Missing Access","code":50001}`, http.StatusForbidden)
			return
		}
		if s.failArmed {
			if s.failAfter == 0 {
				s.failArmed = false
				s.mu.Unlock()
				http.Error(w, `{"message":"Internal Server Error"}`, http.StatusInternalServerError)
				return
			}
			s.failAfter--
		}
		all := s.messages[channelID]
		hook := s.messageHook
		s.mu.Unlock()

		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			limit, _ = strconv.Atoi(raw)
		}
		before := r.URL.Query().Get("before")
		out := []json.RawMessage{}
		for _, m := range all {
			var mm struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(m, &mm)
			if before != "" && !snowflakeLess(mm.ID, before) {
				continue
			}
			out = append(out, m)
			if len(out) == limit {
				break
			}
		}
		if hook != nil {
			hook()
		}
		writeJSON(out)

	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// snowflakeLess compares message IDs numerically, as Discord's "before"
// pagination does.
func snowflakeLess(a, b string) bool {
	ai, errA := strconv.ParseUint(a, 10, 64)
	bi, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		return a < b
	}
	return ai < bi
}

// --- Gateway --------------------------------------------------------------

type gwPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

func (s *Server) handleGateway(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn := &gwConn{ws: ws, ctx: ctx, cancel: cancel}

	s.mu.Lock()
	if s.conn != nil {
		s.conn.cancel()
	}
	s.conn = conn
	s.mu.Unlock()

	conn.send(gwPayload{Op: 10, D: json.RawMessage(fmt.Sprintf(`{"heartbeat_interval":%d}`, heartbeatIntervalMS))})

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			cancel()
			return
		}
		var p gwPayload
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		switch p.Op {
		case 2: // IDENTIFY
			s.mu.Lock()
			s.identifies++
			s.sessionSeq = 0
			session := fmt.Sprintf("sess-%d", s.identifies)
			s.mu.Unlock()
			ready := fmt.Sprintf(`{"v":10,"session_id":%q,"resume_gateway_url":%q,"user":{"id":"bot-user","username":"openconvo","bot":true},"guilds":[]}`, session, s.wsBase())
			s.dispatchTo(conn, "READY", json.RawMessage(ready))
			s.sessionEstablished()

		case 6: // RESUME
			var resume struct {
				Seq int64 `json:"seq"`
			}
			_ = json.Unmarshal(p.D, &resume)
			s.mu.Lock()
			s.resumes++
			s.resumeSeq = resume.Seq
			s.mu.Unlock()
			s.dispatchTo(conn, "RESUMED", json.RawMessage(`{}`))
			s.sessionEstablished()

		case 1: // heartbeat
			s.mu.Lock()
			drop := s.dropACKs
			s.mu.Unlock()
			if !drop {
				conn.send(gwPayload{Op: 11})
			}
		}
	}
}

func (c *gwConn) send(p gwPayload) {
	data, _ := json.Marshal(p)
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	writeCtx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	_ = c.ws.Write(writeCtx, websocket.MessageText, data)
}

func (s *Server) dispatchTo(conn *gwConn, eventType string, data json.RawMessage) {
	s.mu.Lock()
	s.sessionSeq++
	seq := s.sessionSeq
	s.mu.Unlock()
	conn.send(gwPayload{Op: 0, T: eventType, S: &seq, D: data})
}

// sessionEstablished records a completed handshake and wakes waiters.
func (s *Server) sessionEstablished() {
	s.mu.Lock()
	s.sessionsEstablished++
	ch := s.sessionCh
	s.sessionCh = make(chan struct{})
	s.mu.Unlock()
	close(ch)
}

// WaitForSession blocks until a gateway session has been established
// that no earlier WaitForSession call already observed. Sessions that
// established before the call therefore satisfy it immediately, which
// keeps callers free of ordering races.
func (s *Server) WaitForSession(t *testing.T) {
	t.Helper()
	deadline := time.After(sessionWaitTimeout)
	for {
		s.mu.Lock()
		if s.sessionsEstablished > s.sessionsObserved {
			s.sessionsObserved++
			s.mu.Unlock()
			return
		}
		ch := s.sessionCh
		s.mu.Unlock()

		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("no gateway session established within %s", sessionWaitTimeout)
		}
	}
}

// Dispatch pushes an event to the current session.
func (s *Server) Dispatch(t *testing.T, eventType string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		t.Fatal("no gateway connection")
	}
	s.dispatchTo(conn, eventType, raw)
}

// CloseGateway closes the current connection with a Discord close code.
func (s *Server) CloseGateway(code int) {
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.ws.Close(websocket.StatusCode(code), "test close")
		conn.cancel()
	}
}
