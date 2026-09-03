package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/embeddings"
	"github.com/openconvo/openconvo/internal/version"
)

// StatusResponse is the /api/v1/system/status document. It is part of
// the public API surface, so fields are only ever added, not renamed.
type StatusResponse struct {
	Version     version.Info       `json:"version"`
	StartedAt   time.Time          `json:"started_at"`
	Database    DatabaseStatus     `json:"database"`
	Storage     StorageStatus      `json:"storage"`
	Discord     DiscordStatus      `json:"discord"`
	Attachments *AttachmentsStatus `json:"attachments,omitempty"`
	Counts      *CountsStatus      `json:"counts,omitempty"`
	// InsecurePublicAccess reports that the archive has answered a request
	// from outside this network over plain HTTP since it started. It is
	// observed by the server itself rather than assembled with the rest of
	// the status, because only a served request can prove it.
	InsecurePublicAccess bool `json:"insecure_public_access"`
}

// DatabaseStatus describes database connectivity and schema state.
type DatabaseStatus struct {
	Connected     bool   `json:"connected"`
	SchemaVersion int    `json:"schema_version,omitempty"`
	Error         string `json:"error,omitempty"`
}

// StorageStatus describes the attachment blob store.
type StorageStatus struct {
	Driver string `json:"driver"`
	Path   string `json:"path,omitempty"`
}

// DiscordStatus describes Discord configuration and live Gateway state. The
// application ID and bot username are not secrets; credentials are never
// included.
type DiscordStatus struct {
	Configured    bool   `json:"configured"`
	Connected     bool   `json:"connected"`
	ApplicationID string `json:"application_id,omitempty"`
	BotUsername   string `json:"bot_username,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// AttachmentsStatus reports the attachment pipeline's state. Enabled is
// part of the document because "0 stored" means something very
// different when downloading is switched off.
type AttachmentsStatus struct {
	Enabled     bool  `json:"enabled"`
	Stored      int64 `json:"stored"`
	Pending     int64 `json:"pending"`
	Failed      int64 `json:"failed"`
	StoredBytes int64 `json:"stored_bytes"`
}

// CountsStatus reports archive totals.
type CountsStatus struct {
	Communities int64 `json:"communities"`
	Channels    int64 `json:"channels"`
	Messages    int64 `json:"messages"`
	Attachments int64 `json:"attachments"`
}

// healthResponse is the /health document: liveness plus dependency checks.
// It is the one route served without a session, for container healthchecks
// and load balancers, so each check reports only "ok" or "unavailable" — a
// connection error names the host, database and user OpenConvo dials, and
// nothing unauthenticated may learn that. The detail goes to the log, where
// the operator reading it already has it.
type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func handleHealth(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		resp := healthResponse{Status: "ok", Checks: map[string]string{}}
		code := http.StatusOK

		if deps.CheckDatabase != nil {
			if err := deps.CheckDatabase(ctx); err != nil {
				deps.Logger.Error("health check failed", "check", "database", "error", err)
				resp.Status = "degraded"
				resp.Checks["database"] = "unavailable"
				code = http.StatusServiceUnavailable
			} else {
				resp.Checks["database"] = "ok"
			}
		}
		writeJSON(w, code, resp)
	}
}

func handleStatus(deps Deps, exposure *exposureMonitor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Status == nil {
			writeError(w, http.StatusServiceUnavailable, "status not available")
			return
		}
		status, err := deps.Status(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to assemble status")
			return
		}
		if exposure != nil {
			status.InsecurePublicAccess = exposure.insecurePublicAccess()
		}
		writeJSON(w, http.StatusOK, status)
	}
}

// uuidPattern guards path parameters so a malformed ID answers 404
// instead of reaching the database and failing as a 500.
var uuidPattern = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// selectableKinds are the channel kinds an operator can archive
// directly: the ones that hold messages or threads. Kind alone decides
// it — a channel's parent is usually just the category it sits in, and
// threads, which do follow their parent, have kinds of their own.
var selectableKinds = map[string]bool{"text": true, "announcement": true, "forum": true, "media": true}

func requireArchive(deps Deps, w http.ResponseWriter) bool {
	if deps.Archive == nil {
		writeError(w, http.StatusServiceUnavailable, "archive not available")
		return false
	}
	return true
}

func handleListCommunities(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		communities, err := deps.Archive.ListCommunities(r.Context())
		if err != nil {
			deps.Logger.Error("list communities", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list communities")
			return
		}
		if communities == nil {
			communities = []archive.Community{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"communities": communities})
	}
}

func handleListChannels(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		communityID := r.PathValue("id")
		if !uuidPattern.MatchString(communityID) {
			writeError(w, http.StatusNotFound, "community not found")
			return
		}
		channels, err := deps.Archive.ListChannels(r.Context(), communityID)
		if err != nil {
			deps.Logger.Error("list channels", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list channels")
			return
		}
		if channels == nil {
			channels = []archive.Channel{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
	}
}

func handleListArchiveChannels(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		channels, err := deps.Archive.ListArchiveChannels(r.Context())
		if err != nil {
			deps.Logger.Error("list archive channels", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list archive channels")
			return
		}
		if channels == nil {
			channels = []archive.ArchiveChannel{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": channels})
	}
}

func handleListMessages(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		channelID := r.PathValue("id")
		if !uuidPattern.MatchString(channelID) {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		}
		before := r.URL.Query().Get("before")
		if before != "" && !uuidPattern.MatchString(before) {
			writeError(w, http.StatusBadRequest, "before must be a message ID")
			return
		}
		limit, ok := positiveQueryInt(w, r, "limit", 50, 100)
		if !ok {
			return
		}
		channel, found, err := deps.Archive.GetArchiveChannel(r.Context(), channelID)
		if err != nil {
			deps.Logger.Error("load archive channel", "channel_id", channelID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load channel")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		}
		page, err := deps.Archive.ListMessages(r.Context(), channelID, before, limit)
		if errors.Is(err, archive.ErrNotFound) {
			writeError(w, http.StatusBadRequest, "before message is not in this channel")
			return
		}
		if err != nil {
			deps.Logger.Error("list archive messages", "channel_id", channelID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list messages")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"channel":   channel,
			"messages":  page.Messages,
			"has_older": page.HasOlder,
		})
	}
}

func handleMessageContext(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		messageID := r.PathValue("id")
		if !uuidPattern.MatchString(messageID) {
			writeError(w, http.StatusNotFound, "message not found")
			return
		}
		before, ok := nonNegativeQueryInt(w, r, "before", 20, 50)
		if !ok {
			return
		}
		after, ok := nonNegativeQueryInt(w, r, "after", 20, 50)
		if !ok {
			return
		}
		context, found, err := deps.Archive.GetMessageContext(r.Context(), messageID, before, after)
		if err != nil {
			deps.Logger.Error("load message context", "message_id", messageID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load message context")
			return
		}
		if !found {
			writeError(w, http.StatusNotFound, "message not found")
			return
		}
		writeJSON(w, http.StatusOK, context)
	}
}

func handleSearch(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")

		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if query == "" || utf8.RuneCountInString(query) > 500 {
			writeError(w, http.StatusBadRequest, "q is required and must be at most 500 characters")
			return
		}
		mode := strings.TrimSpace(r.URL.Query().Get("mode"))
		if mode == "" {
			mode = "keyword"
		}
		if mode != "keyword" && mode != "semantic" {
			writeError(w, http.StatusBadRequest, "mode must be keyword or semantic")
			return
		}
		channelID := r.URL.Query().Get("channel_id")
		if channelID != "" && !uuidPattern.MatchString(channelID) {
			writeError(w, http.StatusBadRequest, "channel_id must be a UUID")
			return
		}
		author := strings.TrimSpace(r.URL.Query().Get("author"))
		if utf8.RuneCountInString(author) > 200 {
			writeError(w, http.StatusBadRequest, "author must be at most 200 characters")
			return
		}
		after, ok := optionalSearchTime(w, r, "after")
		if !ok {
			return
		}
		before, ok := optionalSearchTime(w, r, "before")
		if !ok {
			return
		}
		if after != nil && before != nil && !after.Before(*before) {
			writeError(w, http.StatusBadRequest, "after must be earlier than before")
			return
		}

		var hasAttachment *bool
		if raw := r.URL.Query().Get("has_attachment"); raw != "" {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "has_attachment must be true or false")
				return
			}
			hasAttachment = &value
		}
		limit, ok := positiveQueryInt(w, r, "limit", 50, 100)
		if !ok {
			return
		}
		offset, ok := nonNegativeQueryInt(w, r, "offset", 0, 100000)
		if !ok {
			return
		}

		params := archive.SearchParams{
			Query: query, ChannelID: channelID, Author: author,
			After: after, Before: before, HasAttachment: hasAttachment,
			Limit: limit, Offset: offset,
		}
		var page archive.SearchPage
		var err error
		if mode == "semantic" {
			if deps.SemanticSearch == nil {
				writeError(w, http.StatusServiceUnavailable, "semantic search is unavailable")
				return
			}
			page, err = deps.SemanticSearch.SearchMessages(r.Context(), params)
		} else {
			page, err = deps.Archive.SearchMessages(r.Context(), params)
		}
		if err != nil {
			switch {
			case errors.Is(err, embeddings.ErrDisabled):
				writeError(w, http.StatusConflict, "semantic search is disabled; enable message embeddings in Settings")
				return
			case errors.Is(err, embeddings.ErrNotConfigured):
				writeError(w, http.StatusServiceUnavailable, "semantic search requires OPENAI_API_KEY")
				return
			case errors.Is(err, embeddings.ErrNotReady):
				writeError(w, http.StatusConflict, "semantic index is still building")
				return
			case errors.Is(err, embeddings.ErrProvider):
				deps.Logger.Warn("semantic search provider failed", "error", err)
				writeError(w, http.StatusBadGateway, "semantic search provider request failed")
				return
			}
			deps.Logger.Error("search archive", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to search archive")
			return
		}
		if page.Results == nil {
			page.Results = []archive.SearchResult{}
		}
		writeJSON(w, http.StatusOK, page)
	}
}

func handleListBookmarks(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		collection := strings.TrimSpace(r.URL.Query().Get("collection"))
		tag := strings.TrimSpace(r.URL.Query().Get("tag"))
		if utf8.RuneCountInString(collection) > 100 || utf8.RuneCountInString(tag) > 50 {
			writeError(w, http.StatusBadRequest, "collection or tag is too long")
			return
		}
		bookmarks, err := deps.Archive.ListBookmarks(r.Context(), archive.BookmarkFilter{
			Collection: collection,
			Tag:        tag,
		})
		if err != nil {
			deps.Logger.Error("list bookmarks", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to list bookmarks")
			return
		}
		if bookmarks == nil {
			bookmarks = []archive.Bookmark{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"bookmarks": bookmarks})
	}
}

func decodeBookmarkInput(w http.ResponseWriter, r *http.Request, requireMessage bool) (archive.BookmarkUpsert, bool) {
	var body struct {
		MessageID   string   `json:"message_id"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Collection  string   `json:"collection"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid bookmark body")
		return archive.BookmarkUpsert{}, false
	}
	body.MessageID = strings.TrimSpace(body.MessageID)
	body.Title = strings.TrimSpace(body.Title)
	body.Description = strings.TrimSpace(body.Description)
	body.Collection = strings.TrimSpace(body.Collection)
	if requireMessage && !uuidPattern.MatchString(body.MessageID) {
		writeError(w, http.StatusBadRequest, "message_id must be a UUID")
		return archive.BookmarkUpsert{}, false
	}
	// An update cannot move a bookmark to another message: create one there
	// instead. Accepting the field and quietly dropping it would tell the
	// caller the move succeeded.
	if !requireMessage && body.MessageID != "" {
		writeError(w, http.StatusBadRequest, "message_id cannot be changed; create a bookmark on the other message")
		return archive.BookmarkUpsert{}, false
	}
	if utf8.RuneCountInString(body.Title) > 200 || utf8.RuneCountInString(body.Description) > 4000 || utf8.RuneCountInString(body.Collection) > 100 {
		writeError(w, http.StatusBadRequest, "title, description, or collection is too long")
		return archive.BookmarkUpsert{}, false
	}
	if len(body.Tags) > 20 {
		writeError(w, http.StatusBadRequest, "a bookmark may have at most 20 tags")
		return archive.BookmarkUpsert{}, false
	}
	seen := make(map[string]bool, len(body.Tags))
	tags := make([]string, 0, len(body.Tags))
	for _, raw := range body.Tags {
		tag := strings.ToLower(strings.TrimSpace(raw))
		if tag == "" {
			continue
		}
		if utf8.RuneCountInString(tag) > 50 {
			writeError(w, http.StatusBadRequest, "tags must be at most 50 characters")
			return archive.BookmarkUpsert{}, false
		}
		if !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return archive.BookmarkUpsert{
		MessageID: body.MessageID, Title: body.Title, Description: body.Description,
		Tags: tags, Collection: body.Collection,
	}, true
}

func handleCreateBookmark(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		input, ok := decodeBookmarkInput(w, r, true)
		if !ok {
			return
		}
		bookmark, created, err := deps.Archive.CreateBookmark(r.Context(), input)
		if errors.Is(err, archive.ErrNotFound) {
			writeError(w, http.StatusNotFound, "message not found")
			return
		}
		if err != nil {
			deps.Logger.Error("create bookmark", "message_id", input.MessageID, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to create bookmark")
			return
		}
		code := http.StatusOK
		if created {
			code = http.StatusCreated
		}
		writeJSON(w, code, map[string]any{"bookmark": bookmark})
	}
}

func handleUpdateBookmark(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		id := r.PathValue("id")
		if !uuidPattern.MatchString(id) {
			writeError(w, http.StatusNotFound, "bookmark not found")
			return
		}
		input, ok := decodeBookmarkInput(w, r, false)
		if !ok {
			return
		}
		bookmark, err := deps.Archive.UpdateBookmark(r.Context(), id, input)
		if errors.Is(err, archive.ErrNotFound) {
			writeError(w, http.StatusNotFound, "bookmark not found")
			return
		}
		if err != nil {
			deps.Logger.Error("update bookmark", "bookmark_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update bookmark")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"bookmark": bookmark})
	}
}

func handleDeleteBookmark(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		id := r.PathValue("id")
		if !uuidPattern.MatchString(id) {
			writeError(w, http.StatusNotFound, "bookmark not found")
			return
		}
		if err := deps.Archive.DeleteBookmark(r.Context(), id); err != nil {
			if errors.Is(err, archive.ErrNotFound) {
				writeError(w, http.StatusNotFound, "bookmark not found")
				return
			}
			deps.Logger.Error("delete bookmark", "bookmark_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to delete bookmark")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func optionalSearchTime(w http.ResponseWriter, r *http.Request, name string) (*time.Time, bool) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return nil, true
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		parsed, err = time.Parse(time.DateOnly, value)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, name+" must be YYYY-MM-DD or RFC3339")
		return nil, false
	}
	return &parsed, true
}

func positiveQueryInt(w http.ResponseWriter, r *http.Request, name string, fallback, maximum int) (int, bool) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 || parsed > maximum {
		writeError(w, http.StatusBadRequest, name+" must be between 1 and "+strconv.Itoa(maximum))
		return 0, false
	}
	return parsed, true
}

func nonNegativeQueryInt(w http.ResponseWriter, r *http.Request, name string, fallback, maximum int) (int, bool) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return fallback, true
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 || parsed > maximum {
		writeError(w, http.StatusBadRequest, name+" must be between 0 and "+strconv.Itoa(maximum))
		return 0, false
	}
	return parsed, true
}

func handleToggleChannel(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		id := r.PathValue("id")
		if !uuidPattern.MatchString(id) {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		}

		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil || body.Enabled == nil {
			writeError(w, http.StatusBadRequest, `body must be {"enabled": true|false}`)
			return
		}

		channel, ok, err := deps.Archive.GetChannel(r.Context(), id)
		if err != nil {
			deps.Logger.Error("load channel", "channel_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load channel")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "channel not found")
			return
		}
		if !selectableKinds[channel.Kind] {
			writeError(w, http.StatusBadRequest,
				"only text, announcement, forum and media channels can be selected; threads follow their parent")
			return
		}

		if err := deps.Archive.SetChannelArchiveEnabled(r.Context(), id, *body.Enabled); err != nil {
			if errors.Is(err, archive.ErrNotFound) {
				writeError(w, http.StatusNotFound, "channel not found")
				return
			}
			deps.Logger.Error("update channel", "channel_id", id, "error", err)
			writeError(w, http.StatusInternalServerError, "failed to update channel")
			return
		}
		if deps.OnChannelToggle != nil {
			if err := deps.OnChannelToggle(r.Context(), id, *body.Enabled); err != nil {
				// The selection itself stuck; startup recovery heals a
				// missed enqueue, but the operator should know.
				deps.Logger.Error("channel toggle follow-up failed", "channel_id", id, "error", err)
				writeError(w, http.StatusInternalServerError, "channel updated but sync scheduling failed")
				return
			}
		}
		channel, _, _ = deps.Archive.GetChannel(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"channel": channel})
	}
}

func handleSyncOverview(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireArchive(deps, w) {
			return
		}
		rows, err := deps.Archive.SyncOverview(r.Context())
		if err != nil {
			deps.Logger.Error("load sync status", "error", err)
			writeError(w, http.StatusInternalServerError, "failed to load sync status")
			return
		}
		if rows == nil {
			rows = []archive.SyncOverviewRow{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"channels": rows})
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, message string) {
	writeJSON(w, code, map[string]string{"error": message})
}
