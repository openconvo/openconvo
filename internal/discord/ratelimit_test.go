package discord

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRouteKeyStripsMinorIDs(t *testing.T) {
	cases := map[string]string{
		"/channels/123/messages":                "GET /channels/123/messages",
		"/channels/123/messages?limit=100":      "GET /channels/123/messages",
		"/guilds/9/channels":                    "GET /guilds/9/channels",
		"/channels/123/messages/456789":         "GET /channels/123/messages/:id",
		"/users/@me":                            "GET /users/@me",
		"/channels/123/threads/archived/public": "GET /channels/123/threads/archived/public",
		"/interactions/123456/token-a/callback": "GET /interactions/:interaction/:token/callback",
		"/interactions/987654/token-b/callback": "GET /interactions/:interaction/:token/callback",
	}
	for path, want := range cases {
		if got := routeKey("GET", path); got != want {
			t.Errorf("routeKey(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestLimiterWaitsWhenBucketExhausted(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset-After", "0.15")
		} else {
			w.Header().Set("X-RateLimit-Remaining", "4")
		}
		_, _ = w.Write([]byte(`{"id":"1","username":"b","bot":true}`))
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	ctx := context.Background()
	if _, err := client.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := client.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("second request not delayed: %s", elapsed)
	}
}

func TestLimiterDoesNotDelayFreshBuckets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "5")
		_, _ = w.Write([]byte(`{"id":"1","username":"b","bot":true}`))
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	start := time.Now()
	for range 3 {
		if _, err := client.CurrentUser(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("fresh buckets delayed: %s", elapsed)
	}
}

func TestLimiterSeparatesRoutes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the /users/@me bucket is exhausted; a different route
		// must not inherit its wait.
		if r.URL.Path == "/users/@me" {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset-After", "30")
		} else {
			w.Header().Set("X-RateLimit-Remaining", "5")
		}
		_, _ = w.Write([]byte(`{"id":"1","username":"b","bot":true}`))
	}))
	defer server.Close()

	client := NewClient("t").WithBaseURL(server.URL)
	ctx := context.Background()
	if _, err := client.CurrentUser(ctx); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := client.GatewayBot(ctx); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("unrelated route delayed by another bucket: %s", elapsed)
	}
}
