package main

import (
	"context"
	"fmt"
	"time"

	"github.com/openconvo/openconvo/internal/archive"
	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/version"
)

func runStatus() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}

	// Status should answer quickly, not retry for half a minute.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info := version.Get()
	fmt.Println("OpenConvo status")
	fmt.Println()
	fmt.Printf("  %-14s %s (%s)\n", "Version", info.Version, info.Commit)

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		// The status table on stdout records the state; the cause travels
		// with the error, which main reports on stderr.
		fmt.Printf("  %-14s unreachable\n", "Database")
		return fmt.Errorf("database unreachable: %w", err)
	}
	defer pool.Close()

	schema, err := database.SchemaVersion(ctx, pool)
	if err != nil {
		return err
	}
	fmt.Printf("  %-14s connected (schema v%d)\n", "Database", schema)
	fmt.Printf("  %-14s %s %s\n", "Storage", cfg.StorageDriver, cfg.StoragePath)
	if cfg.DiscordConfigured() {
		fmt.Printf("  %-14s configured\n", "Discord")
	} else {
		fmt.Printf("  %-14s not configured (set DISCORD_TOKEN)\n", "Discord")
	}

	store := archive.New(pool)
	counts, err := store.Counts(ctx)
	if err != nil {
		return err
	}
	fmt.Println()
	fmt.Printf("  %-14s %d\n", "Communities", counts.Communities)
	fmt.Printf("  %-14s %d\n", "Channels", counts.Channels)
	fmt.Printf("  %-14s %d\n", "Messages", counts.Messages)
	fmt.Printf("  %-14s %d\n", "Attachments", counts.Attachments)

	stats, err := store.AttachmentStats(ctx)
	if err != nil {
		return err
	}
	fmt.Println()
	// "in blobs" rather than "on disk": the total sums blob rows, which
	// includes orphans not yet reclaimed.
	fmt.Printf("  %-14s %d stored, %d pending, %d failed (%s in blobs)\n",
		"Files", stats.Stored, stats.Pending, stats.Failed, humanBytes(stats.StoredBytes))
	switch {
	case !cfg.AttachmentsEnabled && stats.Pending > 0:
		fmt.Printf("  %-14s downloads are off; set OPENCONVO_ATTACHMENTS_ENABLED=true to store these files\n", "")
	case cfg.AttachmentsEnabled && !cfg.DiscordConfigured():
		// Configured on but impossible: expired CDN links can only be
		// refreshed with a token, so nothing would download.
		fmt.Printf("  %-14s downloads are on but cannot run without DISCORD_TOKEN: expired file links cannot be refreshed\n", "")
	}

	rows, err := store.SyncOverview(ctx)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		fmt.Println()
		fmt.Println("  Channels being archived:")
		for _, row := range rows {
			status := row.Status
			if row.BackfillComplete {
				status = "synced"
			}
			fmt.Printf("    #%-24s %-10s %8d messages\n", row.ChannelName, status, row.MessageCount)
		}
	}
	return nil
}

// humanBytes renders a byte count the way an operator reads a disk.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
