-- Recover attachments a bug marked permanently failed.
--
-- The download pipeline used to compare the bytes it fetched against the
-- size in the message metadata and treat any difference as corruption.
-- Discord's CDN re-encodes or rewrites the container of some media on
-- delivery, so that difference is normal and the file was whole every
-- time. After the attempt limit those downloads were recorded as failed
-- with "unavailable at source", which was never true.
--
-- Offering them back to the sweep is enough to heal an existing archive:
-- it re-reads the row, refreshes the URL and downloads again. Only rows
-- carrying that bug's exact verdict are touched, and only failed ones —
-- a stored file is never disturbed. The sweep still applies its own
-- gates, so this cannot resurrect a deleted message's file or reach into
-- a channel the operator has since disabled.
UPDATE attachments
SET download_status = 'pending',
    download_error = NULL,
    updated_at = now()
WHERE download_status = 'failed'
  AND download_error LIKE 'size mismatch: stored % bytes, expected %';
