package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"runtime"
	"time"

	"github.com/coder/websocket"
)

// Gateway opcodes.
const (
	opDispatch       = 0
	opHeartbeat      = 1
	opIdentify       = 2
	opResume         = 6
	opReconnect      = 7
	opInvalidSession = 9
	opHello          = 10
	opHeartbeatACK   = 11
)

// Gateway intents. MESSAGE_CONTENT is privileged: it must be enabled in
// the Discord developer portal or the gateway closes with code 4014.
const (
	intentGuilds                = 1 << 0
	intentGuildMessages         = 1 << 9
	intentGuildMessageReactions = 1 << 10
	intentMessageContent        = 1 << 15

	// ArchiveIntents is everything OpenConvo needs to archive: guild and
	// channel lifecycle, messages, reactions, and message content.
	ArchiveIntents = intentGuilds | intentGuildMessages | intentGuildMessageReactions | intentMessageContent
)

// GatewayEvent is one dispatched Gateway event, raw.
type GatewayEvent struct {
	Type string
	Seq  int64
	Data json.RawMessage
}

// closeAuthenticationFailed is Discord's close code for rejected
// credentials. REST rejects a bad token before a WebSocket can exist, so
// that rejection is reported with the same code.
const closeAuthenticationFailed = 4004

// gatewayAuthLimit is how many consecutive credential rejections from
// GET /gateway/bot count as permanent. A single 401 could be a blip at
// Discord's edge; three across the reconnect backoff is a token that is
// not going to start working.
const gatewayAuthLimit = 3

// FatalGatewayError is a condition that retrying can never fix (bad
// token, disallowed intents, ...), carrying Discord's close code for it.
// Run returns it and stops.
type FatalGatewayError struct {
	Code int
	// Reason is Discord's own words: the close frame's reason, or what
	// the REST rejection said.
	Reason string
}

func (e *FatalGatewayError) Error() string {
	msg := fmt.Sprintf("discord gateway: fatal close code %d", e.Code)
	switch e.Code {
	case closeAuthenticationFailed:
		msg += " (authentication failed — check DISCORD_TOKEN; the bot token is invalid or was regenerated)"
	case 4014:
		msg += " (disallowed intents — enable the Message Content intent for this bot in the Discord developer portal)"
	}
	if e.Reason != "" {
		msg += ": " + e.Reason
	}
	return msg
}

// GatewayOptions configures a Gateway.
type GatewayOptions struct {
	// Intents defaults to ArchiveIntents when zero.
	Intents int
	// Handler receives every dispatch, including READY and RESUMED. A dispatch
	// is acknowledged in the resumable sequence only after Handler succeeds.
	// Returning an error closes the connection and resumes from the preceding
	// sequence so transient archive failures cannot silently lose an event.
	Handler func(GatewayEvent) error
	// OnReidentify is called when a session was lost and replaced:
	// events may have been missed, so callers should reconcile.
	OnReidentify func()
	// OnReady reports an established Gateway session. Username is present
	// after READY and empty after RESUMED, where Discord does not repeat the
	// bot user payload.
	OnReady func(username string)
	// OnDisconnect reports that an established or attempted connection ended.
	// Reconnection remains owned by Gateway; this callback is observability only.
	OnDisconnect func(error)
	Logger       *slog.Logger
}

// Gateway maintains a Discord Gateway connection: identify, heartbeat,
// resume on disconnects, reconnect with backoff. Reconnection is normal
// operation, not an error path.
type Gateway struct {
	client *Client
	token  string
	opts   GatewayOptions
	logger *slog.Logger
}

// NewGateway creates a Gateway. Call Run to connect.
func NewGateway(client *Client, token string, opts GatewayOptions) *Gateway {
	if opts.Intents == 0 {
		opts.Intents = ArchiveIntents
	}
	if opts.Handler == nil {
		opts.Handler = func(GatewayEvent) error { return nil }
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Gateway{client: client, token: token, opts: opts, logger: logger.With("component", "discord.gateway")}
}

type gatewaySession struct {
	id        string
	resumeURL string
	seq       int64
}

type connOutcome int

const (
	outcomeResume connOutcome = iota
	outcomeReidentify
)

type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// Run connects and processes events until ctx is cancelled (returns
// nil) or the connection is unrecoverable — a fatal close code, or
// Discord rejecting the token outright — which returns
// *FatalGatewayError.
func (g *Gateway) Run(ctx context.Context) error {
	var sess *gatewaySession
	attempt := 0
	resumeDialFailures := 0
	authFailures := 0

	for ctx.Err() == nil {
		wsURL := ""
		if sess != nil && sess.resumeURL != "" {
			wsURL = sess.resumeURL
		} else {
			info, err := g.client.GatewayBot(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return nil
				}
				if g.opts.OnDisconnect != nil {
					g.opts.OnDisconnect(err)
				}
				if credentialsRejected(err) {
					// The bad-token case never reaches a close frame: this
					// REST call is where it surfaces, so this is where it
					// has to stop being retried.
					authFailures++
					if authFailures >= gatewayAuthLimit {
						return &FatalGatewayError{
							Code:   closeAuthenticationFailed,
							Reason: fmt.Sprintf("gateway URL rejected %d times in a row: %v", authFailures, err),
						}
					}
				} else {
					authFailures = 0
				}
				g.logger.Warn("fetch gateway url failed", "error", err)
				if !gatewaySleep(ctx, gatewayBackoff(attempt)) {
					return nil
				}
				attempt++
				continue
			}
			authFailures = 0
			wsURL = info.URL
		}

		established, outcome, err := g.runConnection(ctx, wsURL+"?v=10&encoding=json", &sess)
		if ctx.Err() != nil {
			return nil
		}
		if g.opts.OnDisconnect != nil {
			g.opts.OnDisconnect(err)
		}
		var fatal *FatalGatewayError
		if errors.As(err, &fatal) {
			return fatal
		}
		if err != nil {
			g.logger.Warn("gateway connection ended", "error", err)
		}

		if established {
			attempt = 0
			resumeDialFailures = 0
		} else {
			attempt++
			if sess != nil {
				// The resume endpoint may be stale; after repeated
				// failures fall back to a fresh identify.
				resumeDialFailures++
				if resumeDialFailures >= 3 {
					outcome = outcomeReidentify
				}
			}
		}

		if outcome == outcomeReidentify && sess != nil {
			g.logger.Info("gateway session lost; reidentifying")
			sess = nil
			resumeDialFailures = 0
			if g.opts.OnReidentify != nil {
				g.opts.OnReidentify()
			}
		}

		if !gatewaySleep(ctx, gatewayBackoff(attempt)) {
			return nil
		}
	}
	return nil
}

// runConnection handles one WebSocket connection lifecycle.
func (g *Gateway) runConnection(ctx context.Context, wsURL string, sess **gatewaySession) (established bool, outcome connOutcome, err error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, 30*time.Second)
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	cancelDial()
	if err != nil {
		return false, outcomeResume, fmt.Errorf("dial %s: %w", wsURL, err)
	}
	// GUILD_CREATE payloads for large guilds are megabytes.
	conn.SetReadLimit(64 << 20)
	defer conn.CloseNow()

	connCtx, cancelConn := context.WithCancel(ctx)
	defer cancelConn()

	frames := make(chan gatewayPayload)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.Read(connCtx)
			if err != nil {
				readErr <- err
				return
			}
			var p gatewayPayload
			if json.Unmarshal(data, &p) != nil {
				continue
			}
			select {
			case frames <- p:
			case <-connCtx.Done():
				return
			}
		}
	}()

	send := func(op int, d any) error {
		payload, err := json.Marshal(struct {
			Op int `json:"op"`
			D  any `json:"d"`
		}{op, d})
		if err != nil {
			return err
		}
		writeCtx, cancel := context.WithTimeout(connCtx, 10*time.Second)
		defer cancel()
		return conn.Write(writeCtx, websocket.MessageText, payload)
	}

	var heartbeat *time.Timer
	defer func() {
		if heartbeat != nil {
			heartbeat.Stop()
		}
	}()
	var heartbeatC <-chan time.Time
	var interval time.Duration
	acked := true

	currentSeq := func() any {
		if *sess != nil && (*sess).seq > 0 {
			return (*sess).seq
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			// Clean shutdown: 1000 tells Discord to invalidate the session.
			_ = conn.Close(websocket.StatusNormalClosure, "shutting down")
			return established, outcomeResume, nil

		case err := <-readErr:
			status := websocket.CloseStatus(err)
			switch int(status) {
			case closeAuthenticationFailed, 4010, 4011, 4012, 4013, 4014:
				return established, outcomeReidentify, &FatalGatewayError{
					Code: int(status), Reason: closeReason(err),
				}
			case 4007, 4009: // invalid seq / session timed out
				return established, outcomeReidentify, err
			default:
				return established, outcomeResume, err
			}

		case <-heartbeatC:
			if !acked {
				// Zombie connection: close (a non-1000 code keeps the
				// session resumable) and reconnect.
				_ = conn.Close(websocket.StatusServiceRestart, "heartbeat ack missed")
				return established, outcomeResume, fmt.Errorf("heartbeat ack missed")
			}
			if err := send(opHeartbeat, currentSeq()); err != nil {
				return established, outcomeResume, err
			}
			acked = false
			heartbeat.Reset(interval)

		case p := <-frames:
			switch p.Op {
			case opHello:
				var hello struct {
					HeartbeatInterval int `json:"heartbeat_interval"`
				}
				if err := json.Unmarshal(p.D, &hello); err != nil || hello.HeartbeatInterval <= 0 {
					return established, outcomeResume, fmt.Errorf("bad HELLO: %s", p.D)
				}
				interval = time.Duration(hello.HeartbeatInterval) * time.Millisecond
				// The first heartbeat waits interval * jitter, per the docs.
				heartbeat = time.NewTimer(time.Duration(float64(interval) * rand.Float64()))
				heartbeatC = heartbeat.C
				acked = true

				if *sess != nil {
					err = send(opResume, map[string]any{
						"token": g.token, "session_id": (*sess).id, "seq": (*sess).seq,
					})
				} else {
					err = send(opIdentify, map[string]any{
						"token":   g.token,
						"intents": g.opts.Intents,
						"properties": map[string]string{
							"os": runtime.GOOS, "browser": "openconvo", "device": "openconvo",
						},
					})
				}
				if err != nil {
					return established, outcomeResume, err
				}

			case opHeartbeat: // the server requests an immediate heartbeat
				if err := send(opHeartbeat, currentSeq()); err != nil {
					return established, outcomeResume, err
				}

			case opHeartbeatACK:
				acked = true

			case opReconnect:
				_ = conn.Close(websocket.StatusServiceRestart, "server requested reconnect")
				return established, outcomeResume, nil

			case opInvalidSession:
				var resumable bool
				_ = json.Unmarshal(p.D, &resumable)
				// Per the docs: wait 1–5s before the next attempt.
				gatewaySleep(ctx, time.Duration(1+rand.IntN(4))*time.Second)
				if resumable {
					return established, outcomeResume, fmt.Errorf("invalid session (resumable)")
				}
				return established, outcomeReidentify, fmt.Errorf("invalid session")

			case opDispatch:
				if *sess == nil {
					*sess = &gatewaySession{}
				}
				username := ""
				if p.T == "READY" {
					var ready struct {
						SessionID        string `json:"session_id"`
						ResumeGatewayURL string `json:"resume_gateway_url"`
						User             struct {
							Username string `json:"username"`
						} `json:"user"`
					}
					if err := json.Unmarshal(p.D, &ready); err == nil {
						if *sess == nil {
							*sess = &gatewaySession{}
						}
						(*sess).id = ready.SessionID
						(*sess).resumeURL = ready.ResumeGatewayURL
						username = ready.User.Username
						g.logger.Info("gateway ready", "bot", ready.User.Username)
					}
				}
				seq := (*sess).seq
				if p.S != nil {
					seq = *p.S
				}
				if err := g.opts.Handler(GatewayEvent{Type: p.T, Seq: seq, Data: p.D}); err != nil {
					_ = conn.Close(websocket.StatusServiceRestart, "dispatch persistence failed")
					return established, outcomeResume, fmt.Errorf("handle dispatch %s at sequence %d: %w", p.T, seq, err)
				}
				if p.S != nil {
					(*sess).seq = *p.S
				}
				if p.T == "READY" {
					established = true
					if g.opts.OnReady != nil {
						g.opts.OnReady(username)
					}
				}
				if p.T == "RESUMED" {
					g.logger.Info("gateway resumed")
					established = true
					if g.opts.OnReady != nil {
						g.opts.OnReady("")
					}
				}
			}
		}
	}
}

// credentialsRejected reports whether Discord refused our credentials
// outright (401/403), as opposed to failing transiently.
func credentialsRejected(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// closeReason returns the explanation Discord put in the close frame,
// which is the only place it says why it hung up.
func closeReason(err error) string {
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Reason
	}
	return ""
}

// gatewayBackoff paces reconnect attempts: 1s, 2s, 4s ... capped at 60s.
func gatewayBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return time.Second
	}
	d := time.Second << attempt
	if d > time.Minute || d <= 0 {
		return time.Minute
	}
	return d
}

// gatewaySleep sleeps unless ctx ends first; reports whether it slept fully.
func gatewaySleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
