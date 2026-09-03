package discord

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// bucketLimiter tracks Discord's per-route rate-limit state proactively:
// when a route's bucket is exhausted, the next request waits until the
// bucket resets instead of provoking a 429. Reactive 429 handling in
// Client.get remains the backstop. This is the ONLY place OpenConvo
// paces Discord requests.
type bucketLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	remaining int
	resetAt   time.Time
}

func newBucketLimiter() *bucketLimiter {
	return &bucketLimiter{buckets: map[string]*bucket{}}
}

// snowflakePattern matches the numeric IDs Discord embeds in paths.
var snowflakePattern = regexp.MustCompile(`^\d{5,}$`)

// routeKey normalizes a request path into a rate-limit route: the IDs
// directly under /channels/ and /guilds/ are major parameters (each has
// its own bucket); any other numeric segment (e.g. message IDs) is
// collapsed to ":id".
func routeKey(method, path string) string {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(path, "/")
	for i, seg := range segs {
		if seg == "interactions" && i+2 < len(segs) {
			// Each interaction ID/token is one-shot. Keeping either literal
			// would grow the limiter map forever without pacing reusable work.
			segs[i+1] = ":interaction"
			segs[i+2] = ":token"
		}
	}
	for i, seg := range segs {
		if !snowflakePattern.MatchString(seg) {
			continue
		}
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		if prev != "channels" && prev != "guilds" {
			segs[i] = ":id"
		}
	}
	return method + " " + strings.Join(segs, "/")
}

// wait blocks until the route's bucket allows a request (or ctx ends).
func (l *bucketLimiter) wait(ctx context.Context, key string) error {
	l.mu.Lock()
	b, ok := l.buckets[key]
	var sleep time.Duration
	if ok && b.remaining <= 0 {
		sleep = time.Until(b.resetAt)
	}
	l.mu.Unlock()

	if sleep <= 0 {
		return nil
	}
	select {
	case <-time.After(sleep):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// record updates bucket state from response headers.
func (l *bucketLimiter) record(key string, header http.Header) {
	remaining, err := strconv.Atoi(header.Get("X-RateLimit-Remaining"))
	if err != nil {
		return
	}
	resetAfter, _ := strconv.ParseFloat(header.Get("X-RateLimit-Reset-After"), 64)
	l.mu.Lock()
	l.buckets[key] = &bucket{
		remaining: remaining,
		resetAt:   time.Now().Add(time.Duration(resetAfter * float64(time.Second))),
	}
	l.mu.Unlock()
}
