package preservation

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/storage"
)

// VerifyExport validates checksums, counts, record shape, and all references
// without requiring OpenConvo or PostgreSQL.
func VerifyExport(ctx context.Context, root string) (Manifest, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Manifest{}, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		return Manifest{}, fmt.Errorf("verify export: read manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("verify export: decode manifest: %w", err)
	}
	if manifest.Format != FormatName || manifest.FormatVersion != FormatVersion {
		return Manifest{}, fmt.Errorf("verify export: unsupported format %q version %d", manifest.Format, manifest.FormatVersion)
	}
	sums, err := readChecksums(filepath.Join(root, "checksums.sha256"))
	if err != nil {
		return Manifest{}, err
	}
	for _, required := range append([]string{"manifest.json"}, jsonlFiles...) {
		if _, ok := sums[required]; !ok {
			return Manifest{}, fmt.Errorf("verify export: %s is not covered by checksums.sha256", required)
		}
	}
	for _, rendering := range manifest.Renderings {
		if rendering == "markdown" {
			if _, ok := sums[filepath.ToSlash(filepath.Join(markdownRoot, "README.md"))]; !ok {
				return Manifest{}, fmt.Errorf("verify export: Markdown rendering has no checksummed index")
			}
		}
	}
	if err := verifyExportFiles(ctx, root, sums); err != nil {
		return Manifest{}, err
	}

	communities := map[string]struct{}{}
	channels := map[string]struct{}{}
	actors := map[string]struct{}{}
	messages := map[string]struct{}{}
	deletedMessages := map[string]struct{}{}
	attachments := map[string]struct{}{}
	bookmarks := map[string]struct{}{}
	deletions := map[string]struct{}{}
	blobs := map[string]struct{}{}

	communityCount, err := readRecords(filepath.Join(root, "communities.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		return addID("community", line, record.ID, communities)
	})
	if err != nil {
		return Manifest{}, err
	}
	channelCount, err := readRecords(filepath.Join(root, "channels.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID          string `json:"id"`
			CommunityID string `json:"community_id"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if _, ok := communities[record.CommunityID]; !ok {
			return fmt.Errorf("line %d: channel %s references missing community %s", line, record.ID, record.CommunityID)
		}
		return addID("channel", line, record.ID, channels)
	})
	if err != nil {
		return Manifest{}, err
	}
	if err := checkReferenceFile(filepath.Join(root, "channels.jsonl"), "parent_channel_id", channels); err != nil {
		return Manifest{}, err
	}
	actorCount, err := readRecords(filepath.Join(root, "actors.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		return addID("actor", line, record.ID, actors)
	})
	if err != nil {
		return Manifest{}, err
	}
	messageCount, err := readRecords(filepath.Join(root, "messages.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID         string                     `json:"id"`
			ChannelID  string                     `json:"channel_id"`
			ActorID    *string                    `json:"actor_id"`
			Content    *string                    `json:"content"`
			DeletedAt  *string                    `json:"deleted_at"`
			RawPayload map[string]json.RawMessage `json:"raw_payload"`
			Reactions  []struct {
				EmojiKey  string  `json:"emoji_key"`
				MessageID *string `json:"message_id"`
			} `json:"reactions"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if _, ok := channels[record.ChannelID]; !ok {
			return fmt.Errorf("line %d: message %s references missing channel %s", line, record.ID, record.ChannelID)
		}
		if record.ActorID != nil {
			if _, ok := actors[*record.ActorID]; !ok {
				return fmt.Errorf("line %d: message %s references missing actor %s", line, record.ID, *record.ActorID)
			}
		}
		if record.DeletedAt != nil {
			if record.Content != nil || len(record.RawPayload) != 0 {
				return fmt.Errorf("line %d: tombstoned message %s retains content or raw payload", line, record.ID)
			}
			deletedMessages[record.ID] = struct{}{}
		}
		for _, reaction := range record.Reactions {
			// emoji_key is the only identity a reaction needs: it is embedded
			// in the message it belongs to. The row id and message_id
			// OpenConvo also writes are optional, so a producer following
			// docs/archive-format.md verifies; when message_id is present it
			// must still point at the enclosing message.
			if reaction.EmojiKey == "" {
				return fmt.Errorf("line %d: message %s contains a reaction without an emoji_key", line, record.ID)
			}
			if reaction.MessageID != nil && *reaction.MessageID != record.ID {
				return fmt.Errorf("line %d: message %s contains a reaction belonging to message %s", line, record.ID, *reaction.MessageID)
			}
		}
		return addID("message", line, record.ID, messages)
	})
	if err != nil {
		return Manifest{}, err
	}
	if err := checkReferenceFile(filepath.Join(root, "messages.jsonl"), "reply_to_message_id", messages); err != nil {
		return Manifest{}, err
	}
	attachmentCount, err := readRecords(filepath.Join(root, "attachments.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID             string  `json:"id"`
			MessageID      string  `json:"message_id"`
			SHA256         *string `json:"sha256"`
			DownloadStatus string  `json:"download_status"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.ID == "" {
			return fmt.Errorf("line %d: attachment has empty id", line)
		}
		if err := addID("attachment", line, record.ID, attachments); err != nil {
			return err
		}
		if _, ok := messages[record.MessageID]; !ok {
			return fmt.Errorf("line %d: attachment %s references missing message %s", line, record.ID, record.MessageID)
		}
		if _, deleted := deletedMessages[record.MessageID]; deleted {
			return fmt.Errorf("line %d: attachment %s references tombstoned message %s", line, record.ID, record.MessageID)
		}
		if record.DownloadStatus == "stored" && (record.SHA256 == nil || *record.SHA256 == "") {
			return fmt.Errorf("line %d: stored attachment %s has no sha256", line, record.ID)
		}
		if record.DownloadStatus != "stored" && record.SHA256 != nil && *record.SHA256 != "" {
			return fmt.Errorf("line %d: non-stored attachment %s has a sha256", line, record.ID)
		}
		if record.SHA256 != nil && *record.SHA256 != "" {
			if err := storage.ValidateSHA256(*record.SHA256); err != nil {
				return fmt.Errorf("line %d: attachment %s: %w", line, record.ID, err)
			}
			blobs[*record.SHA256] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	bookmarkCount, err := readRecords(filepath.Join(root, "bookmarks.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID        string `json:"id"`
			MessageID string `json:"message_id"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.ID == "" {
			return fmt.Errorf("line %d: bookmark has empty id", line)
		}
		if err := addID("bookmark", line, record.ID, bookmarks); err != nil {
			return err
		}
		if _, ok := messages[record.MessageID]; !ok {
			return fmt.Errorf("line %d: bookmark %s references missing message %s", line, record.ID, record.MessageID)
		}
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	deletionCount, err := readRecords(filepath.Join(root, "deletion_ledger.jsonl"), func(line int, raw []byte) error {
		var record struct {
			ID         string `json:"id"`
			Source     string `json:"source"`
			ObjectType string `json:"object_type"`
			ExternalID string `json:"external_id"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.Source == "" || record.ObjectType == "" || record.ExternalID == "" {
			return fmt.Errorf("line %d: incomplete deletion-ledger identity", line)
		}
		return addID("deletion", line, record.ID, deletions)
	})
	if err != nil {
		return Manifest{}, err
	}

	actual := ManifestCounts{
		Communities:      communityCount,
		Channels:         channelCount,
		Actors:           actorCount,
		Messages:         messageCount,
		Attachments:      attachmentCount,
		Bookmarks:        bookmarkCount,
		Deletions:        deletionCount,
		Blobs:            int64(len(blobs)),
		MarkdownChannels: countMarkdownChannels(sums),
	}
	if actual != manifest.Counts {
		return Manifest{}, fmt.Errorf("verify export: manifest counts %+v do not match records %+v", manifest.Counts, actual)
	}
	for digest := range blobs {
		rel := filepath.ToSlash(filepath.Join("blobs", storage.ObjectKey(digest)))
		if _, ok := sums[rel]; !ok {
			return Manifest{}, fmt.Errorf("verify export: referenced blob %s is missing from checksums", digest)
		}
	}
	for name := range sums {
		if !strings.HasPrefix(name, "blobs/") {
			continue
		}
		digest := filepath.Base(name)
		if storage.ValidateSHA256(digest) != nil || name != filepath.ToSlash(filepath.Join("blobs", storage.ObjectKey(digest))) {
			return Manifest{}, fmt.Errorf("verify export: invalid blob path %s", name)
		}
		// A blob is its own checksum. Without this the content address is
		// unenforced: regenerating checksums.sha256 over corrupted bytes
		// would make the export verify while every attachment pointing at
		// this digest now resolves to different content.
		if sums[name] != digest {
			return Manifest{}, fmt.Errorf("verify export: blob %s is recorded with checksum %s", digest, sums[name])
		}
		if _, ok := blobs[digest]; !ok {
			return Manifest{}, fmt.Errorf("verify export: unreferenced blob %s", digest)
		}
	}
	return manifest, nil
}

// countMarkdownChannels counts the per-channel files of a Markdown
// rendering. Only the index is required by name, so without a count in the
// manifest an export could lose every channel file — checksum lines included
// — and still verify.
func countMarkdownChannels(sums map[string]string) int64 {
	prefix := filepath.ToSlash(filepath.Join(markdownRoot, "channels")) + "/"
	var count int64
	for name := range sums {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".md") {
			count++
		}
	}
	return count
}

func readChecksums(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("verify export: read checksums: %w", err)
	}
	defer file.Close()
	sums := map[string]string{}
	reader := bufio.NewReader(file)
	lineNo := 0
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if len(line) < 67 || line[64:66] != "  " {
				return nil, fmt.Errorf("verify export: checksums line %d is malformed", lineNo)
			}
			digest, name := line[:64], line[66:]
			if err := storage.ValidateSHA256(digest); err != nil {
				return nil, fmt.Errorf("verify export: checksums line %d: %w", lineNo, err)
			}
			if !safeExportPath(name) {
				return nil, fmt.Errorf("verify export: checksums line %d has unsafe path %q", lineNo, name)
			}
			if _, exists := sums[name]; exists {
				return nil, fmt.Errorf("verify export: duplicate checksum path %q", name)
			}
			sums[name] = digest
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	return sums, nil
}

func safeExportPath(name string) bool {
	return name != "" && !strings.Contains(name, "\\") && !filepath.IsAbs(name) && filepath.Clean(name) == name && name != "." && !strings.HasPrefix(name, "../")
}

func verifyExportFiles(ctx context.Context, root string, sums map[string]string) error {
	seen := map[string]struct{}{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("verify export: non-regular file %s", path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "checksums.sha256" {
			return nil
		}
		want, ok := sums[rel]
		if !ok {
			return fmt.Errorf("verify export: file %s is not covered by checksums.sha256", rel)
		}
		got, err := hashFile(path)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("verify export: checksum mismatch for %s: got %s, want %s", rel, got, want)
		}
		seen[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return err
	}
	for name := range sums {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("verify export: checksums references missing file %s", name)
		}
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func readRecords(path string, visit func(line int, raw []byte) error) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("verify export: open %s: %w", filepath.Base(path), err)
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var count int64
	for {
		raw, readErr := reader.ReadBytes('\n')
		if len(raw) > 0 {
			raw = bytesTrimLine(raw)
			count++
			if len(raw) == 0 {
				return 0, fmt.Errorf("verify export: %s line %d is empty", filepath.Base(path), count)
			}
			if err := visit(int(count), raw); err != nil {
				return 0, fmt.Errorf("verify export: %s: %w", filepath.Base(path), err)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return 0, readErr
		}
	}
	return count, nil
}

func bytesTrimLine(raw []byte) []byte {
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
	}
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
	}
	return raw
}

func addID(kind string, line int, id string, set map[string]struct{}) error {
	if id == "" {
		return fmt.Errorf("line %d: %s has empty id", line, kind)
	}
	if _, exists := set[id]; exists {
		return fmt.Errorf("line %d: duplicate %s id %s", line, kind, id)
	}
	set[id] = struct{}{}
	return nil
}

func checkReferenceFile(path, field string, targets map[string]struct{}) error {
	_, err := readRecords(path, func(line int, raw []byte) error {
		var record map[string]json.RawMessage
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		value, ok := record[field]
		if !ok || string(value) == "null" {
			return nil
		}
		var id string
		if err := json.Unmarshal(value, &id); err != nil {
			return fmt.Errorf("line %d: %s is not a string", line, field)
		}
		if _, ok := targets[id]; !ok {
			return fmt.Errorf("line %d: %s references missing id %s", line, field, id)
		}
		return nil
	})
	return err
}

// LiveReport summarizes a live database/blob verification pass.
type LiveReport struct {
	Communities    int64
	Channels       int64
	Actors         int64
	Messages       int64
	Attachments    int64
	Blobs          int64
	HashedBlobs    int64
	Untracked      int64
	StaleTemporary int64
	Removed        int64
	UnknownObjects int64
	Issues         []string
}

func (r LiveReport) Valid() bool { return len(r.Issues) == 0 }

// VerifyLive hashes every database-backed blob, checks logical references,
// and reconciles physical enumeration against blob rows. Repair only removes
// canonical objects with no row and staging files older than staleBefore.
func VerifyLive(ctx context.Context, pool *pgxpool.Pool, blobs storage.Store, repair bool, staleBefore time.Time) (LiveReport, error) {
	var report LiveReport
	if pool == nil || blobs == nil {
		return report, fmt.Errorf("verify live: database and blob store are required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return report, fmt.Errorf("verify live: begin database snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	err = tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM communities),
		(SELECT count(*) FROM channels),
		(SELECT count(*) FROM actors),
		(SELECT count(*) FROM messages),
		(SELECT count(*) FROM attachments),
		(SELECT count(*) FROM blobs)`).Scan(
		&report.Communities, &report.Channels, &report.Actors, &report.Messages, &report.Attachments, &report.Blobs)
	if err != nil {
		return report, fmt.Errorf("verify live: count archive: %w", err)
	}
	var brokenStored, linkedNonStored, deletedResidue int64
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM attachments WHERE download_status = 'stored' AND blob_id IS NULL),
		(SELECT count(*) FROM attachments WHERE download_status <> 'stored' AND blob_id IS NOT NULL),
		(SELECT count(*) FROM messages m WHERE m.deleted_at IS NOT NULL AND
			(m.content IS NOT NULL OR m.raw_payload <> '{}'::jsonb OR
			 EXISTS (SELECT 1 FROM attachments a WHERE a.message_id=m.id) OR
			 EXISTS (SELECT 1 FROM message_reactions r WHERE r.message_id=m.id) OR
			 EXISTS (SELECT 1 FROM bookmarks b WHERE b.message_id=m.id)))`).Scan(
		&brokenStored, &linkedNonStored, &deletedResidue); err != nil {
		return report, fmt.Errorf("verify live: check references: %w", err)
	}
	if brokenStored > 0 {
		report.Issues = append(report.Issues, fmt.Sprintf("%d stored attachments have no blob row", brokenStored))
	}
	if linkedNonStored > 0 {
		report.Issues = append(report.Issues, fmt.Sprintf("%d non-stored attachments still reference a blob", linkedNonStored))
	}
	if deletedResidue > 0 {
		report.Issues = append(report.Issues, fmt.Sprintf("%d tombstoned messages retain canonical or dependent data", deletedResidue))
	}

	known := make(map[string]struct{}, report.Blobs)
	rows, err := tx.Query(ctx, `SELECT sha256, size FROM blobs ORDER BY sha256 FOR SHARE`)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var digest string
		var wantSize int64
		if err := rows.Scan(&digest, &wantSize); err != nil {
			rows.Close()
			return report, err
		}
		known[digest] = struct{}{}
		reader, err := blobs.Open(ctx, digest)
		if err != nil {
			report.Issues = append(report.Issues, fmt.Sprintf("blob %s cannot be opened: %v", digest, err))
			continue
		}
		hasher := sha256.New()
		gotSize, copyErr := io.Copy(hasher, reader)
		closeErr := reader.Close()
		if copyErr != nil || closeErr != nil {
			report.Issues = append(report.Issues, fmt.Sprintf("blob %s cannot be read completely", digest))
			continue
		}
		gotDigest := hex.EncodeToString(hasher.Sum(nil))
		if gotSize != wantSize || gotDigest != digest {
			report.Issues = append(report.Issues, fmt.Sprintf("blob %s has size %d and hash %s; database size is %d", digest, gotSize, gotDigest, wantSize))
			continue
		}
		report.HashedBlobs++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return report, err
	}
	rows.Close()
	if err := tx.Commit(ctx); err != nil {
		return report, fmt.Errorf("verify live: finish database snapshot: %w", err)
	}

	walker, ok := blobs.(storage.ObjectWalker)
	if !ok {
		return report, fmt.Errorf("verify live: storage driver does not support object reconciliation")
	}
	err = walker.WalkObjects(ctx, func(object storage.Object) error {
		switch {
		case object.Temporary:
			if object.Modified.IsZero() || !object.Modified.Before(staleBefore) {
				return nil
			}
			report.StaleTemporary++
			if repair {
				if err := walker.DeleteObject(ctx, object); err != nil {
					return err
				}
				report.Removed++
			}
		case object.SHA256 == "":
			report.UnknownObjects++
			report.Issues = append(report.Issues, fmt.Sprintf("unknown storage object %s", object.Key))
		case hasDigest(known, object.SHA256):
			return nil
		case object.Modified.IsZero() || !object.Modified.Before(staleBefore):
			// A download commits bytes before recording its row. The grace
			// period protects that ordinary in-flight state.
			return nil
		default:
			report.Untracked++
			if repair {
				var exists bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE sha256=$1)`, object.SHA256).Scan(&exists); err != nil {
					return err
				}
				if exists {
					known[object.SHA256] = struct{}{}
					return nil
				}
				if err := walker.DeleteObject(ctx, object); err != nil {
					return err
				}
				report.Removed++
			}
		}
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("verify live: reconcile storage: %w", err)
	}
	if report.Untracked > 0 && !repair {
		report.Issues = append(report.Issues, fmt.Sprintf("%d untracked storage objects need openconvo verify --repair", report.Untracked))
	}
	if report.StaleTemporary > 0 && !repair {
		report.Issues = append(report.Issues, fmt.Sprintf("%d stale temporary files need openconvo verify --repair", report.StaleTemporary))
	}
	sort.Strings(report.Issues)
	return report, nil
}

func hasDigest(set map[string]struct{}, digest string) bool {
	_, ok := set[digest]
	return ok
}
