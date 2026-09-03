package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCurrentUser(t *testing.T) {
	var sawAuth, sawUA atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/users/@me" {
			t.Errorf("path = %s", r.URL.Path)
		}
		sawAuth.Store(r.Header.Get("Authorization"))
		sawUA.Store(r.Header.Get("User-Agent"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "123", "username": "openconvo", "bot": true,
		})
	}))
	defer server.Close()

	client := NewClient("token-abc").WithBaseURL(server.URL)
	user, err := client.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.ID != "123" || !user.Bot {
		t.Errorf("user = %+v", user)
	}
	if sawAuth.Load() != "Bot token-abc" {
		t.Errorf("Authorization = %q", sawAuth.Load())
	}
	if ua, _ := sawUA.Load().(string); ua == "" {
		t.Error("User-Agent not set")
	}
}

func TestRateLimitRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message": "You are being rate limited.", "retry_after": 0.05, "global": false}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "123", "username": "ck", "bot": true})
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	start := time.Now()
	user, err := client.CurrentUser(context.Background())
	if err != nil {
		t.Fatalf("CurrentUser after 429: %v", err)
	}
	if user.ID != "123" {
		t.Errorf("user = %+v", user)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("did not wait for retry_after")
	}
}

func TestRateLimitGivesUpEventually(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"retry_after": 0.001}`))
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	if _, err := client.CurrentUser(context.Background()); err == nil {
		t.Fatal("expected error after exhausting rate limit retries")
	}
}

// A truncated page decodes as broken JSON, and the caller retries the
// same cursor forever. Oversized responses must fail as themselves.
func TestOversizedResponseIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Valid JSON, just past the ceiling.
		_, _ = w.Write([]byte(`[{"id":"1","content":"`))
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxResponseBytes))
		_, _ = w.Write([]byte(`"}]`))
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	_, err := client.ListChannelMessages(context.Background(), "42", "", 100)
	if err == nil {
		t.Fatal("oversized response accepted; a truncated page would retry forever")
	}
	if !strings.Contains(err.Error(), "larger than") {
		t.Errorf("error = %v, want a response-too-large error", err)
	}
}

func TestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "401: Unauthorized", "code": 0}`))
	}))
	defer server.Close()

	client := NewClient("bad-token").WithBaseURL(server.URL)
	_, err := client.CurrentUser(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T (%v), want *APIError", err, err)
	}
	if apiErr.Status != http.StatusUnauthorized {
		t.Errorf("Status = %d", apiErr.Status)
	}
}

func TestEmptyTokenFailsFast(t *testing.T) {
	client := NewClient("  ")
	if _, err := client.CurrentUser(context.Background()); err == nil {
		t.Fatal("expected error with empty token")
	}
}

// An interaction token lets its holder act as the application for 15
// minutes, and it lives in the callback path. No error a request can
// return may carry it, because those errors are logged verbatim.
func TestInteractionTokenNeverReachesErrors(t *testing.T) {
	const token = "interaction-token-must-not-leak"
	const id = "1234567890123456789"
	path := "/interactions/" + id + "/" + token + "/callback"

	// A server that is already closed makes http.Client.Do fail with a
	// *url.Error, which stringifies the whole request URL.
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	undecodable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer undecodable.Close()

	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"retry_after": 0.001}`))
	}))
	defer limited.Close()

	check := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), token) {
			t.Fatalf("interaction token leaked into error: %v", err)
		}
		if !strings.Contains(err.Error(), ":token") {
			t.Errorf("error should name the sanitized route, got: %v", err)
		}
	}

	cases := []struct {
		name    string
		baseURL string
		out     any
	}{
		{"transport failure", closedURL, nil},
		{"undecodable response", undecodable.URL, &struct{}{}},
		{"rate limited", limited.URL, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient("bot-token").WithBaseURL(tc.baseURL)
			check(t, client.requestJSON(context.Background(), http.MethodPost, path,
				map[string]any{}, tc.out, false))
		})
	}

	t.Run("respondInteraction", func(t *testing.T) {
		client := NewClient("bot-token").WithBaseURL(closedURL)
		check(t, client.respondInteraction(context.Background(), id, token, "saved"))
	})
}

func TestGatewayBot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/gateway/bot" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"url":"wss://gateway.discord.gg","shards":1,"session_start_limit":{"total":1000,"remaining":999}}`))
	}))
	defer server.Close()

	info, err := NewClient("t").WithBaseURL(server.URL).GatewayBot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.URL != "wss://gateway.discord.gg" || info.SessionStartLimit.Remaining != 999 {
		t.Errorf("info = %+v", info)
	}
}

// A rejected token never reaches a close frame: the WebSocket handshake
// needs GET /gateway/bot to succeed first. That REST rejection is
// therefore the only place the documented fatal case can be detected.
func TestRejectedTokenIsFatal(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message": "401: Unauthorized", "code": 0}`))
	}))
	defer server.Close()

	const token = "invalid-bot-token"
	gateway := NewGateway(NewClient(token).WithBaseURL(server.URL), token, GatewayOptions{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	err := gateway.Run(ctx)
	var fatal *FatalGatewayError
	if !errors.As(err, &fatal) {
		t.Fatalf("Run error = %v (%T), want *FatalGatewayError", err, err)
	}
	if fatal.Code != closeAuthenticationFailed {
		t.Errorf("Code = %d, want %d", fatal.Code, closeAuthenticationFailed)
	}
	if got := int(calls.Load()); got != gatewayAuthLimit {
		t.Errorf("gateway URL fetches = %d, want %d", got, gatewayAuthLimit)
	}
	if strings.Contains(fatal.Error(), token) {
		t.Errorf("token leaked into fatal error: %v", fatal)
	}
}

func TestListOwnGuildsPaginates(t *testing.T) {
	var afters []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		afters = append(afters, after)
		if after == "" {
			// A full page of 200 means the client must fetch a second page.
			guilds := make([]map[string]any, 200)
			for i := range guilds {
				guilds[i] = map[string]any{"id": fmt.Sprintf("%d", i+1), "name": fmt.Sprintf("g%d", i+1)}
			}
			_ = json.NewEncoder(w).Encode(guilds)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"201","name":"last"}]`))
	}))
	defer server.Close()

	guilds, err := NewClient("t").WithBaseURL(server.URL).ListOwnGuilds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(guilds) != 201 || guilds[200].Name != "last" {
		t.Fatalf("guilds = %d", len(guilds))
	}
	if len(afters) != 2 || afters[1] != "200" {
		t.Errorf("afters = %v", afters)
	}
}

func TestListChannelMessages(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/42/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("before"); got != "999" {
			t.Errorf("before = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "100" {
			t.Errorf("limit = %q", got)
		}
		_, _ = w.Write([]byte(`[{"id":"998"},{"id":"997"}]`))
	}))
	defer server.Close()

	msgs, err := NewClient("t").WithBaseURL(server.URL).ListChannelMessages(context.Background(), "42", "999", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("msgs = %d", len(msgs))
	}
}

func TestListGuildChannelsAndGuild(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/guilds/7":
			_, _ = w.Write([]byte(`{"id":"7","name":"FBFR"}`))
		case "/guilds/7/channels":
			_, _ = w.Write([]byte(`[{"id":"1","type":0},{"id":"2","type":4}]`))
		case "/guilds/7/threads/active":
			_, _ = w.Write([]byte(`{"threads":[{"id":"9","type":11}],"members":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	ctx := context.Background()

	guild, err := client.GetGuild(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	if len(guild) == 0 {
		t.Error("guild payload empty")
	}
	channels, err := client.ListGuildChannels(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 2 {
		t.Errorf("channels = %d", len(channels))
	}
	threads, err := client.ListActiveGuildThreads(ctx, "7")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 {
		t.Errorf("active threads = %d", len(threads))
	}
}

func TestListPublicArchivedThreads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/channels/42/threads/archived/public" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"threads":[{"id":"7"}],"has_more":true}`))
	}))
	defer server.Close()

	threads, more, err := NewClient("t").WithBaseURL(server.URL).ListPublicArchivedThreads(context.Background(), "42", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 1 || !more {
		t.Fatalf("threads=%d more=%v", len(threads), more)
	}
}
