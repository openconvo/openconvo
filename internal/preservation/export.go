// Package preservation implements OpenConvo's durable, source-independent
// export, verification, and deletion-ledger replay tools.
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openconvo/openconvo/internal/storage"
)

const (
	FormatName    = "openconvo-archive"
	FormatVersion = 1
)

var jsonlFiles = []string{
	"communities.jsonl",
	"channels.jsonl",
	"actors.jsonl",
	"messages.jsonl",
	"attachments.jsonl",
	"bookmarks.jsonl",
	"deletion_ledger.jsonl",
}

// Manifest is the stable machine-readable description at the root of every
// OpenConvo export.
type Manifest struct {
	Format           string              `json:"format"`
	FormatVersion    int                 `json:"format_version"`
	GeneratedAt      time.Time           `json:"generated_at"`
	OpenConvoVersion string              `json:"openconvo_version"`
	Sources          []string            `json:"sources"`
	Communities      []ManifestCommunity `json:"communities"`
	Counts           ManifestCounts      `json:"counts"`
	Renderings       []string            `json:"renderings,omitempty"`
}

type ManifestCommunity struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
}

type ManifestCounts struct {
	Communities int64 `json:"communities"`
	Channels    int64 `json:"channels"`
	Actors      int64 `json:"actors"`
	Messages    int64 `json:"messages"`
	Attachments int64 `json:"attachments"`
	Bookmarks   int64 `json:"bookmarks"`
	Deletions   int64 `json:"deletion_ledger"`
	Blobs       int64 `json:"blobs"`
	// MarkdownChannels counts the per-channel files of an included Markdown
	// rendering, so that deleting one cannot go unnoticed. It is absent from
	// exports that carry only the canonical records.
	MarkdownChannels int64 `json:"markdown_channels,omitempty"`
}

// ExportOptions are the dependencies and output settings for one export.
type ExportOptions struct {
	Pool             *pgxpool.Pool
	Blobs            storage.Store
	Destination      string
	OpenConvoVersion string
	RenderMarkdown   bool
	Now              func() time.Time
}

type exportFile struct {
	name  string
	query string
	count *int64
}

// Export creates a complete export in a sibling temporary directory, verifies
// every copied blob as it streams, writes checksums, then atomically renames
// the directory into place. It never leaves a destination that looks complete
// after a failed run.
func Export(ctx context.Context, opts ExportOptions) (Manifest, error) {
	if opts.Pool == nil || opts.Blobs == nil {
		return Manifest{}, fmt.Errorf("export: database and blob store are required")
	}
	destination, err := filepath.Abs(strings.TrimSpace(opts.Destination))
	if err != nil || strings.TrimSpace(opts.Destination) == "" {
		return Manifest{}, fmt.Errorf("export: output directory is required")
	}
	if _, err := os.Lstat(destination); err == nil {
		return Manifest{}, fmt.Errorf("export: destination already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, fmt.Errorf("export: inspect destination: %w", err)
	}
	if filesystem, ok := opts.Blobs.(*storage.Filesystem); ok {
		root, err := filepath.Abs(filesystem.Root())
		if err != nil {
			return Manifest{}, fmt.Errorf("export: resolve storage root: %w", err)
		}
		if withinPath(destination, root) {
			return Manifest{}, fmt.Errorf("export: destination must not be inside the attachment storage root %s", root)
		}
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return Manifest{}, fmt.Errorf("export: create parent directory: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, "."+filepath.Base(destination)+".tmp-")
	if err != nil {
		return Manifest{}, fmt.Errorf("export: create temporary directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(tmp)
		}
	}()

	// The transaction does not modify the database, but it intentionally is
	// not declared READ ONLY: FOR SHARE locks on referenced blob rows keep the
	// reclamation worker from deleting a file while it is being copied.
	tx, err := opts.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return Manifest{}, fmt.Errorf("export: begin snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// to_jsonb renders timestamptz in the session time zone, so an export
	// would otherwise carry whatever zone the server happens to run in.
	// The format promises UTC; SET LOCAL keeps the pooled connection clean.
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`); err != nil {
		return Manifest{}, fmt.Errorf("export: pin snapshot to UTC: %w", err)
	}

	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	manifest := Manifest{
		Format:           FormatName,
		FormatVersion:    FormatVersion,
		GeneratedAt:      now().UTC(),
		OpenConvoVersion: opts.OpenConvoVersion,
		Sources:          []string{},
		Communities:      []ManifestCommunity{},
	}
	if err := loadManifestSummary(ctx, tx, &manifest); err != nil {
		return Manifest{}, err
	}
	if opts.RenderMarkdown {
		manifest.Renderings = []string{"markdown"}
	}

	files := []exportFile{
		{"communities.jsonl", `SELECT to_jsonb(c) FROM communities c ORDER BY c.id`, &manifest.Counts.Communities},
		{"channels.jsonl", `SELECT to_jsonb(ch) FROM channels ch ORDER BY ch.id`, &manifest.Counts.Channels},
		{"actors.jsonl", `SELECT to_jsonb(a) FROM actors a ORDER BY a.id`, &manifest.Counts.Actors},
		{"messages.jsonl", `
			SELECT (to_jsonb(m) - 'search_vector') || jsonb_build_object(
				'reactions', COALESCE((
					SELECT jsonb_agg(to_jsonb(r) ORDER BY r.emoji_key)
					FROM message_reactions r WHERE r.message_id = m.id
				), '[]'::jsonb))
			FROM messages m ORDER BY m.id`, &manifest.Counts.Messages},
		{"attachments.jsonl", `
			SELECT (to_jsonb(a) - 'blob_id') || jsonb_build_object('sha256', b.sha256)
			FROM attachments a LEFT JOIN blobs b ON b.id = a.blob_id ORDER BY a.id`, &manifest.Counts.Attachments},
		{"bookmarks.jsonl", `SELECT to_jsonb(b) FROM bookmarks b ORDER BY b.id`, &manifest.Counts.Bookmarks},
		{"deletion_ledger.jsonl", `SELECT to_jsonb(d) FROM deletion_ledger d ORDER BY d.deleted_at, d.id`, &manifest.Counts.Deletions},
	}
	checksums := make(map[string]string)
	for _, file := range files {
		count, digest, err := writeJSONL(ctx, tx, filepath.Join(tmp, file.name), file.query)
		if err != nil {
			return Manifest{}, fmt.Errorf("export %s: %w", file.name, err)
		}
		*file.count = count
		checksums[file.name] = digest
	}
	if opts.RenderMarkdown {
		channels, err := writeMarkdownRendering(ctx, tx, tmp, manifest, checksums)
		if err != nil {
			return Manifest{}, err
		}
		manifest.Counts.MarkdownChannels = channels
	}

	rows, err := tx.Query(ctx, `
		SELECT b.sha256, b.size
		FROM blobs b
		WHERE EXISTS (SELECT 1 FROM attachments a WHERE a.blob_id = b.id)
		ORDER BY b.sha256
		FOR SHARE OF b`)
	if err != nil {
		return Manifest{}, fmt.Errorf("export: list blobs: %w", err)
	}
	for rows.Next() {
		var digest string
		var size int64
		if err := rows.Scan(&digest, &size); err != nil {
			rows.Close()
			return Manifest{}, fmt.Errorf("export: scan blob: %w", err)
		}
		rel := filepath.ToSlash(filepath.Join("blobs", storage.ObjectKey(digest)))
		if err := copyBlob(ctx, opts.Blobs, filepath.Join(tmp, filepath.FromSlash(rel)), digest, size); err != nil {
			rows.Close()
			return Manifest{}, err
		}
		checksums[rel] = digest
		manifest.Counts.Blobs++
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return Manifest{}, fmt.Errorf("export: list blobs: %w", err)
	}
	rows.Close()

	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return Manifest{}, fmt.Errorf("export: encode manifest: %w", err)
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := writeSyncedFile(filepath.Join(tmp, "manifest.json"), manifestBytes); err != nil {
		return Manifest{}, err
	}
	manifestSum := sha256.Sum256(manifestBytes)
	checksums["manifest.json"] = hex.EncodeToString(manifestSum[:])
	if err := writeChecksums(filepath.Join(tmp, "checksums.sha256"), checksums); err != nil {
		return Manifest{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Manifest{}, fmt.Errorf("export: finish database snapshot: %w", err)
	}
	if err := syncDir(tmp); err != nil {
		return Manifest{}, err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return Manifest{}, fmt.Errorf("export: publish destination: %w", err)
	}
	committed = true
	if err := syncDir(parent); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func withinPath(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func loadManifestSummary(ctx context.Context, tx pgx.Tx, manifest *Manifest) error {
	rows, err := tx.Query(ctx, `SELECT id::text, source, external_id, name FROM communities ORDER BY id`)
	if err != nil {
		return fmt.Errorf("export: list communities: %w", err)
	}
	defer rows.Close()
	sources := map[string]struct{}{}
	for rows.Next() {
		var community ManifestCommunity
		if err := rows.Scan(&community.ID, &community.Source, &community.ExternalID, &community.Name); err != nil {
			return err
		}
		manifest.Communities = append(manifest.Communities, community)
		sources[community.Source] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for source := range sources {
		manifest.Sources = append(manifest.Sources, source)
	}
	sort.Strings(manifest.Sources)
	return nil
}

func writeJSONL(ctx context.Context, tx pgx.Tx, path, query string) (int64, string, error) {
	rows, err := tx.Query(ctx, query)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, "", err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, "", err
	}
	hasher := sha256.New()
	writer := bufio.NewWriter(io.MultiWriter(file, hasher))
	var count int64
	for rows.Next() {
		var record []byte
		if err := rows.Scan(&record); err != nil {
			file.Close()
			return 0, "", err
		}
		if _, err := writer.Write(record); err != nil {
			file.Close()
			return 0, "", err
		}
		if err := writer.WriteByte('\n'); err != nil {
			file.Close()
			return 0, "", err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		file.Close()
		return 0, "", err
	}
	if err := writer.Flush(); err != nil {
		file.Close()
		return 0, "", err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return 0, "", err
	}
	if err := file.Close(); err != nil {
		return 0, "", err
	}
	return count, hex.EncodeToString(hasher.Sum(nil)), nil
}

func copyBlob(ctx context.Context, blobs storage.Store, path, expectedDigest string, expectedSize int64) error {
	if err := storage.ValidateSHA256(expectedDigest); err != nil {
		return fmt.Errorf("export: invalid blob digest in database: %w", err)
	}
	reader, err := blobs.Open(ctx, expectedDigest)
	if err != nil {
		return fmt.Errorf("export: open blob %s: %w", expectedDigest, err)
	}
	defer reader.Close()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hasher), reader)
	if copyErr == nil {
		copyErr = ctx.Err()
	}
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("export: copy blob %s: %w", expectedDigest, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("export: close blob %s: %w", expectedDigest, closeErr)
	}
	got := hex.EncodeToString(hasher.Sum(nil))
	if size != expectedSize || got != expectedDigest {
		return fmt.Errorf("export: blob %s failed verification: size %d (want %d), sha256 %s", expectedDigest, size, expectedSize, got)
	}
	return nil
}

func writeChecksums(path string, sums map[string]string) error {
	names := make([]string, 0, len(sums))
	for name := range sums {
		names = append(names, name)
	}
	sort.Strings(names)
	var body strings.Builder
	for _, name := range names {
		if strings.ContainsAny(name, "\r\n") {
			return fmt.Errorf("export: invalid checksum path %q", name)
		}
		fmt.Fprintf(&body, "%s  %s\n", sums[name], name)
	}
	return writeSyncedFile(path, []byte(body.String()))
}

func writeSyncedFile(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
