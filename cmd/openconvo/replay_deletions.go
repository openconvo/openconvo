package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/preservation"
)

const replayDeletionsUsage = `usage: openconvo replay-deletions [--unverified] [--yes] <export-directory|deletion_ledger.jsonl>`

const replayDeletionsHelp = replayDeletionsUsage + `

Reapplies the deletions recorded in an export's ledger to a database restored
from an older backup: the messages it names are tombstoned and the
attachments, channels and communities it names are deleted — a channel or
community takes its contents with it.

An export directory is checked against its manifest before anything is
applied. A bare deletion_ledger.jsonl has no manifest vouching for it, so
replaying one requires --unverified.

This cannot be undone without restoring a database backup. It is confirmed on
the terminal unless --yes is given.`

func runReplayDeletions(args []string) error {
	flags := flag.NewFlagSet("replay-deletions", flag.ContinueOnError)
	unverified := flags.Bool("unverified", false, "accept a bare deletion ledger file that no export manifest vouches for")
	assumeYes := flags.Bool("yes", false, "skip the confirmation prompt (for scripts)")
	if done, err := parseFlags(flags, replayDeletionsHelp, args); done {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New(replayDeletionsUsage)
	}
	path := flags.Arg(0)

	// Decide what the path is before connecting: a bare ledger file is
	// applied without any verification, so it takes an explicit opt-in.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	kind := "export directory, verified against its manifest first"
	if !info.IsDir() {
		if !*unverified {
			return fmt.Errorf("%s is not an export directory: a bare deletion ledger cannot be checked against an export manifest, so applying one requires --unverified", path)
		}
		kind = "unverified ledger file"
	}

	fmt.Printf("About to replay the deletion ledger from %s (%s)\n", path, kind)
	fmt.Println("Every message it names is tombstoned and its content erased, and the")
	fmt.Println("attachments, channels and communities it names are deleted along with")
	fmt.Println("their contents. This cannot be undone without restoring a backup.")
	if err := confirmDestructive("Apply these deletions?", *assumeYes); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.RequireDatabase(); err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	report, err := preservation.ReplayDeletions(ctx, pool, path)
	if err != nil {
		return err
	}
	fmt.Printf("replayed %d deletion entries: %d messages tombstoned, %d actors scrubbed, %d attachments, %d channels, and %d communities removed\n",
		report.LedgerEntries, report.MessagesTombstoned, report.ActorsScrubbed,
		report.AttachmentsDeleted, report.ChannelsDeleted, report.CommunitiesDeleted)
	return nil
}
