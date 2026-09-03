// Package discord is the Discord source: the REST client every Discord
// request goes through — the one place rate limiting lives — the
// Gateway connection that streams live events, and normalization of
// Discord payloads into the canonical archive model. Backfill and
// reconciliation are scheduled by internal/syncer on top of this
// client; ingestion of what arrives is internal/ingest's.
//
// OpenConvo only ever authenticates as an official Discord bot
// application: every request authorizes as "Bot <token>", which a user
// token cannot satisfy. Self-bots and user-account automation are
// prohibited by Discord and unsupported here.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/openconvo/openconvo/internal/version"
)

// DefaultBaseURL is the Discord REST API base.
const DefaultBaseURL = "https://discord.com/api/v10"

// maxResponseBytes caps one REST response body. The largest thing
// OpenConvo asks Discord for is a 100-message page, and 8 MiB leaves
// ~80 KiB per message — far above a real one, even with the maximum
// embeds, components and a nested referenced_message — while still
// bounding what a single response can allocate.
const maxResponseBytes = 8 << 20

// Client is the single, centralized Discord HTTP client. All REST
// traffic goes through it so rate-limit handling lives in exactly one
// place — never as ad-hoc sleeps around the codebase. It honors 429
// responses (Retry-After / retry_after) with bounded retries.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
	// maxRetries applies to rate-limit (429) responses only.
	maxRetries int
	// limiter paces requests per route from Discord's own headers, so
	// normal operation stays below the limits instead of hitting them.
	limiter *bucketLimiter
}

// NewClient creates a Discord REST client for the given bot token.
func NewClient(token string) *Client {
	return &Client{
		token:      strings.TrimSpace(token),
		baseURL:    DefaultBaseURL,
		http:       &http.Client{Timeout: 30 * time.Second},
		maxRetries: 5,
		limiter:    newBucketLimiter(),
	}
}

// WithBaseURL overrides the API base URL (tests).
func (c *Client) WithBaseURL(url string) *Client {
	c.baseURL = strings.TrimRight(url, "/")
	return c
}

// APIError is a non-2xx Discord API response.
type APIError struct {
	Status  int
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("discord API error: HTTP %d, code %d: %s", e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("discord API error: HTTP %d", e.Status)
}

// get performs a GET request and decodes the JSON response into out.
func (c *Client) get(ctx context.Context, path string, out any) error {
	if c.token == "" {
		return fmt.Errorf("discord: no bot token configured")
	}

	// routeKey doubles as the label errors report: it replaces the
	// interaction ID and token with placeholders, so a live credential
	// cannot travel out of here inside an error string.
	key := routeKey(http.MethodGet, path)

	for attempt := 0; ; attempt++ {
		if err := c.limiter.wait(ctx, key); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+c.token)
		req.Header.Set("User-Agent", userAgent())
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("discord: request %s: %w", key, transportError(err))
		}
		body, err := readBody(resp)
		if err != nil {
			return fmt.Errorf("discord: read response from %s: %w", key, err)
		}
		c.limiter.record(key, resp.Header)

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("discord: decode response from %s: %w", key, err)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests:
			if attempt >= c.maxRetries {
				return fmt.Errorf("discord: rate limited on %s after %d retries", key, attempt)
			}
			wait := retryAfter(resp.Header, body)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}

		default:
			apiErr := &APIError{Status: resp.StatusCode}
			_ = json.Unmarshal(body, apiErr) // best effort; keep HTTP status regardless
			return apiErr
		}
	}
}

// post performs a POST request with a JSON body and decodes the JSON
// response into out. It shares get's rate-limit bookkeeping so every
// route's budget is tracked in one place.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.requestJSON(ctx, http.MethodPost, path, body, out, true)
}

// requestJSON sends a mutating JSON request through the same route limiter as
// every other Discord REST call. Interaction callbacks are webhook endpoints
// and therefore opt out of bot authorization, but never out of centralized
// rate-limit handling. Their path embeds an interaction token — a credential
// that can act as the application — so errors name the sanitized route
// instead of the path they were built from.
func (c *Client) requestJSON(ctx context.Context, method, path string, body, out any, authenticated bool) error {
	if c.token == "" {
		if authenticated {
			return fmt.Errorf("discord: no bot token configured")
		}
	}

	key := routeKey(method, path)
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("discord: encode request for %s: %w", key, err)
	}

	for attempt := 0; ; attempt++ {
		if err := c.limiter.wait(ctx, key); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if authenticated {
			req.Header.Set("Authorization", "Bot "+c.token)
		}
		req.Header.Set("User-Agent", userAgent())
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("discord: request %s: %w", key, transportError(err))
		}
		respBody, err := readBody(resp)
		if err != nil {
			return fmt.Errorf("discord: read response from %s: %w", key, err)
		}
		c.limiter.record(key, resp.Header)

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			if out == nil {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("discord: decode response from %s: %w", key, err)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests:
			if attempt >= c.maxRetries {
				return fmt.Errorf("discord: rate limited on %s after %d retries", key, attempt)
			}
			wait := retryAfter(resp.Header, respBody)
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}

		default:
			apiErr := &APIError{Status: resp.StatusCode}
			_ = json.Unmarshal(respBody, apiErr)
			return apiErr
		}
	}
}

// readBody reads a response body under a hard ceiling. io.ReadAll over a
// plain io.LimitReader cannot tell "the body ended" from "the limit was
// reached": an oversized response came back truncated with a nil error,
// so the caller saw a JSON decode failure, retried the identical cursor
// and stalled that channel's history forever. Reading one byte past the
// ceiling turns truncation into an error that says what happened.
//
// A body within the ceiling is read to EOF, which is what leaves the
// connection reusable. Past it the connection is dropped instead:
// draining an unbounded remainder to save one socket is a bad trade.
func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response larger than %d bytes", maxResponseBytes)
	}
	return body, nil
}

// transportError strips the request URL out of a transport failure.
// *url.Error stringifies the whole URL it was building a request for,
// which for an interaction callback is a live credential; the caller
// already reports the sanitized route.
func transportError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		return urlErr.Err
	}
	return err
}

// retryAfter extracts the wait duration from a 429 response, preferring
// the JSON body's retry_after (seconds, possibly fractional), falling
// back to the Retry-After header, then to one second.
func retryAfter(header http.Header, body []byte) time.Duration {
	var payload struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.RetryAfter > 0 {
		return time.Duration(payload.RetryAfter * float64(time.Second))
	}
	if s := header.Get("Retry-After"); s != "" {
		if secs, err := strconv.ParseFloat(s, 64); err == nil && secs > 0 {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return time.Second
}

func userAgent() string {
	return fmt.Sprintf("DiscordBot (https://github.com/openconvo/openconvo, %s)", version.Version)
}

// BotUser is the authenticated bot account.
type BotUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Bot      bool   `json:"bot"`
}

// CurrentUser returns the bot account the token belongs to. This is the
// cheapest way to validate a token.
func (c *Client) CurrentUser(ctx context.Context) (BotUser, error) {
	var user BotUser
	err := c.get(ctx, "/users/@me", &user)
	return user, err
}

// GatewayBotInfo is the response of GET /gateway/bot: the WebSocket URL
// to connect to and the session start budget.
type GatewayBotInfo struct {
	URL               string `json:"url"`
	Shards            int    `json:"shards"`
	SessionStartLimit struct {
		Total     int `json:"total"`
		Remaining int `json:"remaining"`
	} `json:"session_start_limit"`
}

// GatewayBot returns the Gateway connection info for this bot.
func (c *Client) GatewayBot(ctx context.Context) (GatewayBotInfo, error) {
	var info GatewayBotInfo
	err := c.get(ctx, "/gateway/bot", &info)
	return info, err
}

// PartialGuild is a guild as returned by GET /users/@me/guilds.
type PartialGuild struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Icon string `json:"icon"`
}

// guildPageSize is the maximum page size of GET /users/@me/guilds.
const guildPageSize = 200

// ListOwnGuilds returns every guild the bot is a member of, following
// pagination.
func (c *Client) ListOwnGuilds(ctx context.Context) ([]PartialGuild, error) {
	var all []PartialGuild
	after := ""
	for {
		path := fmt.Sprintf("/users/@me/guilds?limit=%d", guildPageSize)
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		var page []PartialGuild
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < guildPageSize {
			return all, nil
		}
		after = page[len(page)-1].ID
	}
}

// GetGuild returns the raw guild object.
func (c *Client) GetGuild(ctx context.Context, guildID string) (json.RawMessage, error) {
	var raw json.RawMessage
	err := c.get(ctx, "/guilds/"+url.PathEscape(guildID), &raw)
	return raw, err
}

// ListGuildChannels returns the guild's channels as raw objects, so
// normalization owns the interpretation and raw_payload keeps everything.
func (c *Client) ListGuildChannels(ctx context.Context, guildID string) ([]json.RawMessage, error) {
	var raw []json.RawMessage
	err := c.get(ctx, "/guilds/"+url.PathEscape(guildID)+"/channels", &raw)
	return raw, err
}

// ListChannelMessages returns up to limit messages, newest first,
// strictly before the given message ID (empty = latest messages).
// Requires the READ_MESSAGE_HISTORY permission.
func (c *Client) ListChannelMessages(ctx context.Context, channelID, before string, limit int) ([]json.RawMessage, error) {
	path := fmt.Sprintf("/channels/%s/messages?limit=%d", url.PathEscape(channelID), limit)
	if before != "" {
		path += "&before=" + url.QueryEscape(before)
	}
	var raw []json.RawMessage
	err := c.get(ctx, path, &raw)
	return raw, err
}

// ListActiveGuildThreads returns the guild's active threads (raw).
func (c *Client) ListActiveGuildThreads(ctx context.Context, guildID string) ([]json.RawMessage, error) {
	var resp struct {
		Threads []json.RawMessage `json:"threads"`
	}
	err := c.get(ctx, "/guilds/"+url.PathEscape(guildID)+"/threads/active", &resp)
	return resp.Threads, err
}

// ListPublicArchivedThreads returns one page of a channel's archived
// public threads. before is an ISO8601 archive timestamp ("" = newest);
// callers paginate with the last thread's archive timestamp while
// hasMore is true.
func (c *Client) ListPublicArchivedThreads(ctx context.Context, channelID, before string) ([]json.RawMessage, bool, error) {
	path := "/channels/" + url.PathEscape(channelID) + "/threads/archived/public?limit=100"
	if before != "" {
		path += "&before=" + url.QueryEscape(before)
	}
	var resp struct {
		Threads []json.RawMessage `json:"threads"`
		HasMore bool              `json:"has_more"`
	}
	err := c.get(ctx, path, &resp)
	return resp.Threads, resp.HasMore, err
}
