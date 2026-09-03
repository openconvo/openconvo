package preservation

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/openconvo/openconvo/internal/storage"
)

const markdownRoot = "markdown"

type markdownChannel struct {
	ID              string
	CommunityID     string
	CommunityName   string
	ParentChannelID *string
	ParentName      string
	Kind            string
	Name            string
	Topic           string
	Position        int
	IsPrivate       bool
	IsArchived      bool
	MessageCount    int64
	ParentRendered  bool
}

type markdownMessage struct {
	ID               string
	Content          *string
	SourceCreatedAt  time.Time
	AuthorName       string
	AuthorIsBot      bool
	ReplyToMessageID *string
	ReplyChannelID   *string
	Attachments      []markdownAttachment
	Reactions        []markdownReaction
	Bookmarks        []markdownBookmark
}

type markdownAttachment struct {
	Filename       string  `json:"filename"`
	Description    string  `json:"description"`
	Size           int64   `json:"size"`
	DownloadStatus string  `json:"download_status"`
	SHA256         *string `json:"sha256"`
}

type markdownReaction struct {
	EmojiKey  string `json:"emoji_key"`
	EmojiName string `json:"emoji_name"`
	Count     int    `json:"count"`
}

type markdownBookmark struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Collection  string   `json:"collection"`
}

// writeMarkdownRendering adds a human-readable view to the canonical export.
// The JSONL records remain the authoritative archive; every Markdown file is
// derived from the same repeatable-read snapshot and covered by its checksums.
// It returns the number of per-channel files written, which the manifest
// records so that a lost channel file is detectable.
func writeMarkdownRendering(ctx context.Context, tx pgx.Tx, root string, manifest Manifest, checksums map[string]string) (int64, error) {
	channels, err := loadMarkdownChannels(ctx, tx)
	if err != nil {
		return 0, fmt.Errorf("export markdown: %w", err)
	}

	index := renderMarkdownIndex(manifest, channels)
	indexName := filepath.ToSlash(filepath.Join(markdownRoot, "README.md"))
	if err := os.MkdirAll(filepath.Join(root, markdownRoot), 0o755); err != nil {
		return 0, fmt.Errorf("export markdown: create rendering directory: %w", err)
	}
	if err := writeSyncedFile(filepath.Join(root, filepath.FromSlash(indexName)), []byte(index)); err != nil {
		return 0, fmt.Errorf("export markdown: write index: %w", err)
	}
	checksums[indexName] = hashBytes([]byte(index))

	for _, channel := range channels {
		name := markdownChannelPath(channel.ID)
		digest, err := writeMarkdownChannel(ctx, tx, filepath.Join(root, filepath.FromSlash(name)), channel)
		if err != nil {
			return 0, fmt.Errorf("export markdown channel %s: %w", channel.ID, err)
		}
		checksums[name] = digest
	}
	return int64(len(channels)), nil
}

func loadMarkdownChannels(ctx context.Context, tx pgx.Tx) ([]markdownChannel, error) {
	rows, err := tx.Query(ctx, `
		SELECT ch.id::text, ch.community_id::text, c.name,
			ch.parent_channel_id::text, COALESCE(parent.name, ''),
			ch.kind, ch.name, ch.topic, ch.position, ch.is_private, ch.is_archived,
			(SELECT count(*) FROM messages m WHERE m.channel_id=ch.id AND m.deleted_at IS NULL)
		FROM channels ch
		JOIN communities c ON c.id=ch.community_id
		LEFT JOIN channels parent ON parent.id=ch.parent_channel_id
		WHERE EXISTS (SELECT 1 FROM messages m WHERE m.channel_id=ch.id AND m.deleted_at IS NULL)
		ORDER BY c.id, ch.position, ch.source_created_at NULLS LAST, ch.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []markdownChannel
	for rows.Next() {
		var channel markdownChannel
		if err := rows.Scan(
			&channel.ID, &channel.CommunityID, &channel.CommunityName,
			&channel.ParentChannelID, &channel.ParentName,
			&channel.Kind, &channel.Name, &channel.Topic, &channel.Position,
			&channel.IsPrivate, &channel.IsArchived, &channel.MessageCount,
		); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rendered := make(map[string]struct{}, len(channels))
	for _, channel := range channels {
		rendered[channel.ID] = struct{}{}
	}
	for i := range channels {
		if channels[i].ParentChannelID != nil {
			_, channels[i].ParentRendered = rendered[*channels[i].ParentChannelID]
		}
	}
	return channels, nil
}

func renderMarkdownIndex(manifest Manifest, channels []markdownChannel) string {
	byCommunity := make(map[string][]markdownChannel)
	for _, channel := range channels {
		byCommunity[channel.CommunityID] = append(byCommunity[channel.CommunityID], channel)
	}

	var body strings.Builder
	fmt.Fprintln(&body, "# OpenConvo Markdown archive")
	fmt.Fprintln(&body)
	fmt.Fprintln(&body, "This is a human-readable rendering of the canonical JSONL archive in the parent directory.")
	fmt.Fprintln(&body)
	fmt.Fprintf(&body, "Generated: %s\n\n", manifest.GeneratedAt.UTC().Format(time.RFC3339))

	for _, community := range manifest.Communities {
		fmt.Fprintf(&body, "## %s\n\n", markdownInline(displayName(community.Name, "Unnamed community")))
		communityChannels := byCommunity[community.ID]
		if len(communityChannels) == 0 {
			fmt.Fprintln(&body, "_No archived messages._")
			fmt.Fprintln(&body)
			continue
		}
		for _, channel := range communityChannels {
			label := "#" + displayName(channel.Name, "unnamed-channel")
			details := channel.Kind
			if channel.ParentChannelID != nil {
				details += " in #" + displayName(channel.ParentName, "unnamed-channel")
			}
			if channel.IsPrivate {
				details += ", private"
			}
			if channel.IsArchived {
				details += ", archived"
			}
			fmt.Fprintf(&body, "- [%s](channels/%s.md) — %s; %d messages\n",
				markdownInline(label), channel.ID, markdownInline(details), channel.MessageCount)
		}
		fmt.Fprintln(&body)
	}
	return body.String()
}

func writeMarkdownChannel(ctx context.Context, tx pgx.Tx, path string, channel markdownChannel) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	hasher := sha256.New()
	buffer := bufio.NewWriter(io.MultiWriter(file, hasher))
	writer := markdownWriter{writer: buffer}

	writer.printf("# #%s\n\n", markdownInline(displayName(channel.Name, "unnamed-channel")))
	writer.printf("- **Community:** %s\n", markdownInline(displayName(channel.CommunityName, "Unnamed community")))
	writer.printf("- **Type:** %s\n", markdownInline(channel.Kind))
	if channel.ParentChannelID != nil {
		parentName := markdownInline("#" + displayName(channel.ParentName, "unnamed-channel"))
		if channel.ParentRendered {
			writer.printf("- **Parent:** [%s](%s.md)\n", parentName, *channel.ParentChannelID)
		} else {
			writer.printf("- **Parent:** %s\n", parentName)
		}
	}
	if channel.Topic != "" {
		writer.printf("- **Topic:** %s\n", markdownInline(singleLine(channel.Topic)))
	}
	writer.printf("- **Messages:** %d\n\n", channel.MessageCount)
	writer.printf("---\n\n")

	rows, queryErr := tx.Query(ctx, `
		SELECT m.id::text, m.content, m.source_created_at,
			COALESCE(NULLIF(a.display_name, ''), NULLIF(a.username, ''), 'Unknown author'),
			COALESCE(a.is_bot, false), m.reply_to_message_id::text, reply.channel_id::text,
			COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'filename', att.filename, 'description', att.description,
					'size', COALESCE(b.size, att.size), 'download_status', att.download_status,
					'sha256', b.sha256) ORDER BY att.id)
				FROM attachments att LEFT JOIN blobs b ON b.id=att.blob_id
				WHERE att.message_id=m.id
			), '[]'::jsonb),
			COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'emoji_key', r.emoji_key, 'emoji_name', r.emoji_name, 'count', r.count)
					ORDER BY r.emoji_key)
				FROM message_reactions r WHERE r.message_id=m.id
			), '[]'::jsonb),
			COALESCE((
				SELECT jsonb_agg(jsonb_build_object(
					'title', b.title, 'description', b.description,
					'tags', b.tags, 'collection', b.collection) ORDER BY b.created_at, b.id)
				FROM bookmarks b WHERE b.message_id=m.id
			), '[]'::jsonb)
		FROM messages m
		LEFT JOIN actors a ON a.id=m.actor_id
		LEFT JOIN messages reply ON reply.id=m.reply_to_message_id AND reply.deleted_at IS NULL
		WHERE m.channel_id=$1::uuid AND m.deleted_at IS NULL
		ORDER BY m.source_created_at, m.id`, channel.ID)
	if queryErr == nil {
		for rows.Next() {
			var message markdownMessage
			var attachments, reactions, bookmarks []byte
			if err := rows.Scan(
				&message.ID, &message.Content, &message.SourceCreatedAt,
				&message.AuthorName, &message.AuthorIsBot,
				&message.ReplyToMessageID, &message.ReplyChannelID,
				&attachments, &reactions, &bookmarks,
			); err != nil {
				queryErr = err
				break
			}
			if err := json.Unmarshal(attachments, &message.Attachments); err != nil {
				queryErr = fmt.Errorf("decode attachments for message %s: %w", message.ID, err)
				break
			}
			if err := json.Unmarshal(reactions, &message.Reactions); err != nil {
				queryErr = fmt.Errorf("decode reactions for message %s: %w", message.ID, err)
				break
			}
			if err := json.Unmarshal(bookmarks, &message.Bookmarks); err != nil {
				queryErr = fmt.Errorf("decode bookmarks for message %s: %w", message.ID, err)
				break
			}
			renderMarkdownMessage(&writer, channel.ID, message)
			if writer.err != nil {
				queryErr = writer.err
				break
			}
		}
		if err := rows.Err(); queryErr == nil && err != nil {
			queryErr = err
		}
		rows.Close()
	}

	if queryErr == nil {
		queryErr = writer.err
	}
	if queryErr == nil {
		queryErr = buffer.Flush()
	}
	if queryErr == nil {
		queryErr = file.Sync()
	}
	closeErr := file.Close()
	if queryErr != nil {
		return "", queryErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func renderMarkdownMessage(writer *markdownWriter, channelID string, message markdownMessage) {
	author := markdownInline(displayName(message.AuthorName, "Unknown author"))
	if message.AuthorIsBot {
		author += " (bot)"
	}
	writer.printf("<a id=\"message-%s\"></a>\n", message.ID)
	writer.printf("## %s — %s\n\n", author, message.SourceCreatedAt.UTC().Format(time.RFC3339))
	writer.printf("Message ID: `%s`\n\n", message.ID)
	if message.ReplyToMessageID != nil {
		if message.ReplyChannelID == nil {
			writer.printf("Reply to message `%s` (not present in this rendering).\n\n", *message.ReplyToMessageID)
		} else if *message.ReplyChannelID == channelID {
			writer.printf("Reply to [message `%s`](#message-%s).\n\n", *message.ReplyToMessageID, *message.ReplyToMessageID)
		} else {
			writer.printf("Reply to [message `%s`](%s.md#message-%s).\n\n",
				*message.ReplyToMessageID, *message.ReplyChannelID, *message.ReplyToMessageID)
		}
	}

	for _, bookmark := range message.Bookmarks {
		title := displayName(bookmark.Title, "Saved message")
		writer.printf("**Bookmark:** %s", markdownInline(title))
		if bookmark.Collection != "" {
			writer.printf(" · Collection: %s", markdownInline(singleLine(bookmark.Collection)))
		}
		if len(bookmark.Tags) > 0 {
			tags := make([]string, 0, len(bookmark.Tags))
			for _, tag := range bookmark.Tags {
				tags = append(tags, markdownInline(singleLine(tag)))
			}
			writer.printf(" · Tags: %s", strings.Join(tags, ", "))
		}
		writer.printf("\n\n")
		if bookmark.Description != "" {
			writer.printf("Bookmark note:\n\n")
			writer.literal(bookmark.Description)
			writer.printf("\n")
		}
	}

	if message.Content == nil {
		writer.printf("_No textual content._\n")
	} else {
		writer.literal(*message.Content)
	}

	if len(message.Attachments) > 0 {
		writer.printf("\n**Attachments**\n\n")
		for _, attachment := range message.Attachments {
			filename := markdownInline(displayName(attachment.Filename, "attachment"))
			if attachment.DownloadStatus == "stored" && attachment.SHA256 == nil {
				writer.fail(fmt.Errorf("message %s stored attachment %q has no digest", message.ID, attachment.Filename))
				return
			}
			if attachment.DownloadStatus == "stored" {
				if err := storage.ValidateSHA256(*attachment.SHA256); err != nil {
					writer.fail(fmt.Errorf("message %s attachment %q: %w", message.ID, attachment.Filename, err))
					return
				}
				object := filepath.ToSlash(filepath.Join("..", "..", "blobs", storage.ObjectKey(*attachment.SHA256)))
				writer.printf("- [%s](%s) — %d bytes", filename, object, attachment.Size)
			} else {
				writer.printf("- %s — %s, %d bytes", filename, markdownInline(attachment.DownloadStatus), attachment.Size)
			}
			if attachment.Description != "" {
				writer.printf(" — %s", markdownInline(singleLine(attachment.Description)))
			}
			writer.printf("\n")
		}
	}

	if len(message.Reactions) > 0 {
		parts := make([]string, 0, len(message.Reactions))
		for _, reaction := range message.Reactions {
			emoji := displayName(reaction.EmojiName, reaction.EmojiKey)
			parts = append(parts, fmt.Sprintf("%s ×%d", markdownInline(singleLine(emoji)), reaction.Count))
		}
		writer.printf("\n**Reactions:** %s\n", strings.Join(parts, ", "))
	}
	writer.printf("\n---\n\n")
}

type markdownWriter struct {
	writer io.Writer
	err    error
}

func (writer *markdownWriter) printf(format string, args ...any) {
	if writer.err != nil {
		return
	}
	_, writer.err = fmt.Fprintf(writer.writer, format, args...)
}

func (writer *markdownWriter) literal(value string) {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	for _, line := range strings.Split(value, "\n") {
		writer.printf("    %s\n", line)
	}
}

func (writer *markdownWriter) fail(err error) {
	if writer.err == nil {
		writer.err = err
	}
}

func markdownChannelPath(id string) string {
	return filepath.ToSlash(filepath.Join(markdownRoot, "channels", id+".md"))
}

func displayName(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// markdownInline renders untrusted source text — names, filenames, topics —
// as a single line of literal Markdown. Escaping alone is not enough: a name
// carrying a newline would end the line it was placed in and let the rest
// open a heading, blockquote, or fence of its own.
func markdownInline(value string) string {
	var escaped strings.Builder
	for _, char := range value {
		switch char {
		case '\n', '\r':
			escaped.WriteByte(' ')
			continue
		case '\\', '`', '*', '_', '{', '}', '[', ']', '<', '>', '&', '#', '+', '-', '.', '!', '|':
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}
	return escaped.String()
}

func hashBytes(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}
