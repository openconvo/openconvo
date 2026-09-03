// Package archive defines the canonical archive model and its
// PostgreSQL persistence. This is the heart of OpenConvo.
//
// Everything in this package is source-agnostic: rows are identified on
// their platform of origin by (source, external_id), never by
// platform-specific columns. Discord is the first source, not a special
// case. Search indexes, exports, and any future intelligence layers are
// derived from — and rebuildable from — the data managed here.
package archive

import (
	"encoding/json"
	"time"
)

// SourceDiscord is the source identifier for Discord. Defined here so
// the archive package never imports a source implementation.
const SourceDiscord = "discord"

// Community is a container of channels: a Discord guild today,
// potentially a Slack workspace or forum later.
type Community struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	ExternalID  string          `json:"external_id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	IconURL     string          `json:"icon_url"`
	RawPayload  json.RawMessage `json:"raw_payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Channel is a text channel, forum channel, or thread. Threads are
// channels with a ParentChannelID.
type Channel struct {
	ID              string          `json:"id"`
	CommunityID     string          `json:"community_id"`
	ExternalID      string          `json:"external_id"`
	ParentChannelID *string         `json:"parent_channel_id,omitempty"`
	Kind            string          `json:"kind"`
	Name            string          `json:"name"`
	Topic           string          `json:"topic"`
	Position        int             `json:"position"`
	IsPrivate       bool            `json:"is_private"`
	IsArchived      bool            `json:"is_archived"`
	ArchiveEnabled  bool            `json:"archive_enabled"`
	SourceCreatedAt *time.Time      `json:"source_created_at,omitempty"`
	RawPayload      json.RawMessage `json:"raw_payload,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// Actor is a message author.
type Actor struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	ExternalID  string          `json:"external_id"`
	Username    string          `json:"username"`
	DisplayName string          `json:"display_name"`
	AvatarURL   string          `json:"avatar_url"`
	IsBot       bool            `json:"is_bot"`
	RawPayload  json.RawMessage `json:"raw_payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// Message is a single archived message. Content is nil for deleted
// (tombstoned) messages.
type Message struct {
	ID                string          `json:"id"`
	ChannelID         string          `json:"channel_id"`
	ActorID           *string         `json:"actor_id,omitempty"`
	ExternalID        string          `json:"external_id"`
	Kind              string          `json:"kind"`
	Content           *string         `json:"content"`
	ReplyToMessageID  *string         `json:"reply_to_message_id,omitempty"`
	ReplyToExternalID *string         `json:"reply_to_external_id,omitempty"`
	SourceCreatedAt   time.Time       `json:"source_created_at"`
	SourceUpdatedAt   *time.Time      `json:"source_updated_at,omitempty"`
	DeletedAt         *time.Time      `json:"deleted_at,omitempty"`
	RawPayload        json.RawMessage `json:"raw_payload,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// Deleted reports whether the message has been tombstoned.
func (m *Message) Deleted() bool { return m.DeletedAt != nil }

// Reaction is an aggregated reaction on a message.
type Reaction struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	EmojiKey  string `json:"emoji_key"`
	EmojiName string `json:"emoji_name"`
	Count     int    `json:"count"`
}

// StoredAttachment is the metadata needed to serve one archived attachment.
// The physical object is addressed by SHA256; Filename and ContentType remain
// canonical archive metadata rather than properties of a storage driver.
type StoredAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
}

// Counts summarizes archive size for dashboards and status output.
type Counts struct {
	Communities int64 `json:"communities"`
	Channels    int64 `json:"channels"`
	Messages    int64 `json:"messages"`
	Attachments int64 `json:"attachments"`
	Blobs       int64 `json:"blobs"`
}

// Deletion ledger object types.
const (
	ObjectTypeMessage    = "message"
	ObjectTypeActor      = "actor" // replay compatibility; v1 has no producer command
	ObjectTypeChannel    = "channel"
	ObjectTypeCommunity  = "community"
	ObjectTypeAttachment = "attachment"
)

// Synchronization statuses of a channel.
const (
	SyncStatusPending   = "pending"
	SyncStatusImporting = "importing"
	SyncStatusSynced    = "synced"
	SyncStatusError     = "error"
	SyncStatusDisabled  = "disabled"
)

// SyncState is a channel's synchronization progress. The external ID
// checkpoints make historical import resumable: an interrupted backfill
// continues where it stopped instead of starting over.
type SyncState struct {
	ID               string     `json:"id"`
	ChannelID        string     `json:"channel_id"`
	Status           string     `json:"status"`
	OldestExternalID *string    `json:"oldest_external_id,omitempty"`
	NewestExternalID *string    `json:"newest_external_id,omitempty"`
	BackfillComplete bool       `json:"backfill_complete"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	CompletedAt      *time.Time `json:"completed_at,omitempty"`
	LastSyncedAt     *time.Time `json:"last_synced_at,omitempty"`
	LastError        string     `json:"last_error"`
}

// SyncOverviewRow reports one archived channel's sync state for the
// dashboard and CLI status output.
type SyncOverviewRow struct {
	ChannelID        string     `json:"channel_id"`
	ChannelName      string     `json:"channel_name"`
	CommunityName    string     `json:"community_name"`
	Kind             string     `json:"kind"`
	Status           string     `json:"status"`
	BackfillComplete bool       `json:"backfill_complete"`
	LastSyncedAt     *time.Time `json:"last_synced_at,omitempty"`
	LastError        string     `json:"last_error,omitempty"`
	MessageCount     int64      `json:"message_count"`
}

// ArchiveChannel is the read-only channel summary used by the archive UI.
// ParentKind distinguishes a thread parent from a category parent without
// making either concept source-specific.
type ArchiveChannel struct {
	ID                string     `json:"id"`
	CommunityID       string     `json:"community_id"`
	CommunityName     string     `json:"community_name"`
	ParentChannelID   *string    `json:"parent_channel_id,omitempty"`
	ParentChannelName string     `json:"parent_channel_name,omitempty"`
	ParentKind        string     `json:"parent_kind,omitempty"`
	Kind              string     `json:"kind"`
	Name              string     `json:"name"`
	Topic             string     `json:"topic"`
	Position          int        `json:"position"`
	IsPrivate         bool       `json:"is_private"`
	IsArchived        bool       `json:"is_archived"`
	ArchiveEnabled    bool       `json:"archive_enabled"`
	SyncStatus        string     `json:"sync_status"`
	BackfillComplete  bool       `json:"backfill_complete"`
	MessageCount      int64      `json:"message_count"`
	LastMessageAt     *time.Time `json:"last_message_at,omitempty"`
}

// ArchiveActor is the durable identity displayed beside a message. It omits
// source payloads and other persistence details that the browsing UI does not
// need.
type ArchiveActor struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	IsBot       bool   `json:"is_bot"`
}

// ArchiveAttachment is attachment metadata safe for archive readers. In
// particular it never exposes the expiring source URL or a blob object key.
type ArchiveAttachment struct {
	ID             string `json:"id"`
	Filename       string `json:"filename"`
	Description    string `json:"description,omitempty"`
	ContentType    string `json:"content_type,omitempty"`
	Size           int64  `json:"size"`
	DownloadStatus string `json:"download_status"`
}

// MessageSticker is a sticker sent in place of message text. A message can
// carry stickers and no content at all, so a reader given only the content
// has nothing to show for it. Only the source's own identifier and name are
// exposed: the artwork is not archived, and the name is the durable fact.
type MessageSticker struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// MessageReference is the compact reply preview displayed above a message.
// Kind and Stickers are here for the same reason they are on ArchiveMessage:
// a referenced message may have no text, and a preview that can only read
// Content would have to present an archived message as missing.
type MessageReference struct {
	ID              string           `json:"id"`
	Kind            string           `json:"kind"`
	Content         *string          `json:"content"`
	Stickers        []MessageSticker `json:"stickers"`
	Actor           *ArchiveActor    `json:"actor,omitempty"`
	SourceCreatedAt time.Time        `json:"source_created_at"`
}

// ArchiveMessage is a normalized, deletion-safe read model for one message.
// Tombstones are intentionally absent from archive reads.
type ArchiveMessage struct {
	ID              string              `json:"id"`
	ChannelID       string              `json:"channel_id"`
	ExternalID      string              `json:"external_id"`
	Kind            string              `json:"kind"`
	Content         *string             `json:"content"`
	Stickers        []MessageSticker    `json:"stickers"`
	Actor           *ArchiveActor       `json:"actor,omitempty"`
	ReplyTo         *MessageReference   `json:"reply_to,omitempty"`
	SourceCreatedAt time.Time           `json:"source_created_at"`
	SourceUpdatedAt *time.Time          `json:"source_updated_at,omitempty"`
	Attachments     []ArchiveAttachment `json:"attachments"`
	Reactions       []Reaction          `json:"reactions"`
	BookmarkID      *string             `json:"bookmark_id,omitempty"`
}

// Bookmark is durable, operator-authored curation metadata attached to one
// live archived message. Collection is a lightweight grouping name; tags are
// normalized strings. The compact message fields make bookmark listings useful
// without exposing source payloads or performing one query per bookmark.
type Bookmark struct {
	ID              string        `json:"id"`
	MessageID       string        `json:"message_id"`
	Title           string        `json:"title"`
	Description     string        `json:"description"`
	Tags            []string      `json:"tags"`
	Collection      string        `json:"collection"`
	ChannelID       string        `json:"channel_id"`
	ChannelName     string        `json:"channel_name"`
	CommunityName   string        `json:"community_name"`
	Content         *string       `json:"content"`
	Actor           *ArchiveActor `json:"actor,omitempty"`
	SourceCreatedAt time.Time     `json:"source_created_at"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// BookmarkUpsert is the editable bookmark metadata accepted from the admin UI.
type BookmarkUpsert struct {
	MessageID   string
	Title       string
	Description string
	Tags        []string
	Collection  string
}

// BookmarkFilter narrows the bookmark listing. Empty fields mean all values.
type BookmarkFilter struct {
	Collection string
	Tag        string
}

// MessagePage is one chronological page from a channel timeline. Before is a
// stable message-ID cursor; HasOlder tells the UI whether to offer another
// page without relying on mutable channel names or timestamps in URLs.
type MessagePage struct {
	Messages []ArchiveMessage `json:"messages"`
	HasOlder bool             `json:"has_older"`
}

// MessageContext is a target message with its surrounding conversation.
type MessageContext struct {
	Channel  ArchiveChannel   `json:"channel"`
	TargetID string           `json:"target_id"`
	Messages []ArchiveMessage `json:"messages"`
}

// SearchParams describes a derived archive query. Keyword and semantic search
// share these canonical-message filters without making any field part of
// message identity or the write path.
type SearchParams struct {
	Query         string
	ChannelID     string
	Author        string
	After         *time.Time
	Before        *time.Time
	HasAttachment *bool
	Limit         int
	Offset        int
}

// SearchResult is a compact message hit. Keyword excerpts may contain <mark>
// delimiters inserted by PostgreSQL's ts_headline; semantic excerpts are
// bounded plain message text. Clients must treat all other text as plain text.
type SearchResult struct {
	MessageID       string        `json:"message_id"`
	ChannelID       string        `json:"channel_id"`
	ChannelName     string        `json:"channel_name"`
	CommunityName   string        `json:"community_name"`
	Actor           *ArchiveActor `json:"actor,omitempty"`
	SourceCreatedAt time.Time     `json:"source_created_at"`
	Excerpt         string        `json:"excerpt"`
	HasAttachment   bool          `json:"has_attachment"`
}

// SearchPage is one relevance-ranked page of search results.
type SearchPage struct {
	Results []SearchResult `json:"results"`
	HasMore bool           `json:"has_more"`
}
