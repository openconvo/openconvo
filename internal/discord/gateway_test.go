package discord_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openconvo/openconvo/internal/discord"
	"github.com/openconvo/openconvo/internal/discord/discordtest"
)

type eventCollector struct {
	mu     sync.Mutex
	events []discord.GatewayEvent
}

func (c *eventCollector) handle(ev discord.GatewayEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *eventCollector) types() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.events))
	for i, ev := range c.events {
		out[i] = ev.Type
	}
	return out
}

func (c *eventCollector) waitFor(t *testing.T, eventType string) discord.GatewayEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, ev := range c.events {
			if ev.Type == eventType {
				c.mu.Unlock()
				return ev
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s not received; got %v", eventType, c.types())
	return discord.GatewayEvent{}
}

func testGatewayLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startGateway runs a Gateway against the fake until the test ends. The
// done channel is closed after the run result is sent, so a test may
// read the result itself without deadlocking the cleanup.
func startGateway(t *testing.T, s *discordtest.Server, opts discord.GatewayOptions) (context.CancelFunc, chan error) {
	t.Helper()
	client := discord.NewClient("test-token").WithBaseURL(s.BaseURL())
	if opts.Logger == nil {
		opts.Logger = testGatewayLogger()
	}
	gw := discord.NewGateway(client, "test-token", opts)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- gw.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return cancel, done
}

func TestGatewayConnectAndDispatch(t *testing.T) {
	s := discordtest.New(t)
	collector := &eventCollector{}
	cancel, done := startGateway(t, s, discord.GatewayOptions{Handler: collector.handle})

	s.WaitForSession(t)
	collector.waitFor(t, "READY")
	s.Dispatch(t, "MESSAGE_CREATE", map[string]any{"id": "42", "channel_id": "c1"})
	ev := collector.waitFor(t, "MESSAGE_CREATE")
	if ev.Seq == 0 || len(ev.Data) == 0 {
		t.Errorf("event = %+v", ev)
	}
	if s.Identifies() != 1 {
		t.Errorf("identifies = %d, want 1", s.Identifies())
	}

	cancel()
	if err := <-done; err != nil {
		t.Errorf("Run returned %v on clean shutdown", err)
	}
}

func TestGatewayReportsConnectionState(t *testing.T) {
	s := discordtest.New(t)
	ready := make(chan string, 1)
	disconnected := make(chan error, 1)
	_, done := startGateway(t, s, discord.GatewayOptions{
		OnReady:      func(username string) { ready <- username },
		OnDisconnect: func(err error) { disconnected <- err },
	})

	s.WaitForSession(t)
	select {
	case username := <-ready:
		if username != "openconvo" {
			t.Errorf("username = %q, want openconvo", username)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Gateway did not report READY")
	}

	s.CloseGateway(4014)
	select {
	case err := <-disconnected:
		var fatal *discord.FatalGatewayError
		if !errors.As(err, &fatal) || fatal.Code != 4014 {
			t.Fatalf("disconnect error = %v, want FatalGatewayError 4014", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Gateway did not report disconnect")
	}

	if err := <-done; err == nil {
		t.Fatal("Run returned nil after fatal close")
	}
}

func TestGatewayResumesAfterServerClose(t *testing.T) {
	s := discordtest.New(t)
	collector := &eventCollector{}
	startGateway(t, s, discord.GatewayOptions{Handler: collector.handle})

	s.WaitForSession(t)
	collector.waitFor(t, "READY")

	// A non-fatal close means the client reconnects with RESUME, not
	// IDENTIFY: the session survives.
	s.CloseGateway(4000)
	s.WaitForSession(t)
	collector.waitFor(t, "RESUMED")
	if s.Resumes() != 1 {
		t.Errorf("resumes = %d, want 1", s.Resumes())
	}
	if s.Identifies() != 1 {
		t.Errorf("identifies = %d, want still 1", s.Identifies())
	}
}

func TestGatewayDoesNotAcknowledgeFailedDispatch(t *testing.T) {
	s := discordtest.New(t)
	collector := &eventCollector{}
	failed := make(chan int64, 1)
	startGateway(t, s, discord.GatewayOptions{Handler: func(ev discord.GatewayEvent) error {
		if ev.Type == "MESSAGE_CREATE" {
			select {
			case failed <- ev.Seq:
				return errors.New("database unavailable")
			default:
			}
		}
		return collector.handle(ev)
	}})

	s.WaitForSession(t)
	ready := collector.waitFor(t, "READY")
	s.Dispatch(t, "MESSAGE_CREATE", map[string]any{"id": "42", "channel_id": "c1"})

	var failedSeq int64
	select {
	case failedSeq = <-failed:
	case <-time.After(10 * time.Second):
		t.Fatal("failed dispatch was not handled")
	}
	s.WaitForSession(t)
	if failedSeq <= ready.Seq {
		t.Fatalf("failed sequence = %d, ready sequence = %d", failedSeq, ready.Seq)
	}
	if got := s.ResumeSequence(); got != ready.Seq {
		t.Errorf("RESUME sequence = %d, want last durable sequence %d (failed event was %d)", got, ready.Seq, failedSeq)
	}
}

func TestGatewayReidentifiesOnInvalidSession(t *testing.T) {
	s := discordtest.New(t)
	collector := &eventCollector{}
	var reidentified sync.WaitGroup
	reidentified.Add(1)
	startGateway(t, s, discord.GatewayOptions{
		Handler:      collector.handle,
		OnReidentify: func() { reidentified.Done() },
	})

	s.WaitForSession(t)
	collector.waitFor(t, "READY")

	// 4007 (invalid sequence) is not resumable: a fresh identify plus the
	// resync callback, because events may have been missed.
	s.CloseGateway(4007)
	s.WaitForSession(t)
	waitTimeout(t, &reidentified, 10*time.Second)
	if s.Identifies() != 2 {
		t.Errorf("identifies = %d, want 2", s.Identifies())
	}
}

func TestGatewayZombieDetection(t *testing.T) {
	s := discordtest.New(t)
	collector := &eventCollector{}
	startGateway(t, s, discord.GatewayOptions{Handler: collector.handle})

	s.WaitForSession(t)
	collector.waitFor(t, "READY")

	// Stop ACKing heartbeats: the client must notice and reconnect.
	s.DropHeartbeatACKs(true)
	s.WaitForSession(t) // a new session (resume) must establish
	s.DropHeartbeatACKs(false)
	if s.Resumes() < 1 {
		t.Errorf("resumes = %d, want >= 1", s.Resumes())
	}
}

func TestGatewayFatalCloseCode(t *testing.T) {
	s := discordtest.New(t)
	collector := &eventCollector{}
	_, done := startGateway(t, s, discord.GatewayOptions{Handler: collector.handle})

	s.WaitForSession(t)
	collector.waitFor(t, "READY")
	s.CloseGateway(4014) // disallowed intents — retrying can never help
	select {
	case err := <-done:
		var fatal *discord.FatalGatewayError
		if !errors.As(err, &fatal) || fatal.Code != 4014 {
			t.Fatalf("err = %v, want FatalGatewayError 4014", err)
		}
		// The message must tell the operator how to fix it.
		if !strings.Contains(fatal.Error(), "Message Content") {
			t.Errorf("fatal error unhelpful: %s", fatal)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return on fatal close code")
	}
}

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal("timeout waiting")
	}
}
