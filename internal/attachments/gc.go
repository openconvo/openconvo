package attachments

import (
	"context"
	"errors"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/jobs"
)

const (
	// gcGrace is how long a blob is left alone after creation. A
	// download stores its file and links it moments later; without this
	// window, reclamation could take the file away in between.
	gcGrace = time.Hour
	// gcBatch bounds one reclamation pass.
	gcBatch = 500
)

// HandleGC deletes blobs nothing references any more, one at a time via
// reclaim, which carries the ordering that makes each deletion safe.
func (d *Downloader) HandleGC(ctx context.Context, _ *jobs.Job) error {
	orphans, err := d.store.ListOrphanBlobs(ctx, time.Now().Add(-gcGrace), gcBatch)
	if err != nil {
		return err
	}

	removed := 0
	for _, blob := range orphans {
		ok, err := d.reclaim(ctx, blob)
		if err != nil {
			return err
		}
		if ok {
			removed++
		}
	}

	if removed > 0 {
		d.logger.Info("reclaimed orphaned blobs", "count", removed)
	}
	return nil
}

// reclaim removes one orphaned blob, row first and file second, and
// reports whether it removed anything.
//
// The row goes first because ON DELETE RESTRICT makes that step a live
// re-check: if an attachment has come to reference this blob since it
// was listed, the delete fails, the file is still untouched, and the
// blob keeps both its row and its bytes. Deleting the file first (the
// original design) does not have this protection — a concurrent
// download can dedupe onto the file, then link a fresh reference to the
// row, and the file would already be gone by the time that reference is
// noticed.
//
// A second check follows the row delete: a download can also lose the
// race on the row itself. Once this blob's row is gone, EnsureBlob has
// nothing left to conflict with on the digest, so it inserts a new row
// for the same content — one that points at the very file reclaim is
// about to remove. BlobExistsBySHA catches that and leaves the file
// alone.
//
// Interrupted, raced, or failed after that check passes, this leaks a
// file with no row pointing at it — a plain error return from
// BlobExistsBySHA or from the file delete leaks in exactly the same
// shape as a crash does. That costs wasted disk, but nothing is lost and
// nothing is resurrected, and the bytes are adopted automatically if
// identical content is ever archived again (Put deduplicates onto the
// file, EnsureBlob recreates a row). The reverse order has no such
// recovery: deleting the file before the row can destroy bytes a live
// reference still needs.
//
// The gap between the digest check and the file delete below is not
// locked, so it narrows this race rather than closing it outright. The
// cheapest way to close it, if it ever proves worth closing, is to wrap
// DeleteBlob and the file delete in one transaction: blobs.sha256 is
// unique, so a concurrent EnsureBlob for the same digest blocks on the
// uncommitted delete until commit, by which point the download's own
// post-store Exists check (download.go) sees the file is gone and
// retries. No advisory lock and no coordination with the download path.
// The caveat is that it holds a transaction open across a storage
// delete: an unlink with the filesystem driver, but a network round trip
// with the S3 one, where a slow or retrying bucket would pin a database
// connection for as long as it takes.
func (d *Downloader) reclaim(ctx context.Context, blob archive.OrphanBlob) (bool, error) {
	if err := d.store.DeleteBlob(ctx, blob.ID); err != nil {
		// Referenced again since it was listed, or already gone. Either
		// way this blob is not ours to reclaim, and its file stays.
		if errors.Is(err, archive.ErrBlobReferenced) || errors.Is(err, archive.ErrNotFound) {
			return false, nil
		}
		return false, err
	}

	// The row is gone, but a download may have deduplicated onto this
	// exact content and inserted a fresh row for the same digest,
	// pointing at this very file. Leave the bytes alone if so.
	readopted, err := d.store.BlobExistsBySHA(ctx, blob.SHA256)
	if err != nil {
		return false, err
	}
	if readopted {
		return false, nil
	}

	if err := d.blobs.Delete(ctx, blob.SHA256); err != nil {
		return false, err
	}
	return true, nil
}
