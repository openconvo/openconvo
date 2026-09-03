package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/preservation"
)

const verifyUsage = `usage: openconvo verify [--repair] [<export-directory>]`

const verifyHelp = verifyUsage + `

Checks archived data against its own recorded hashes and counts. Given an
export directory, it checks that export against its manifest; given no
argument, it checks the live archive and cross-checks its references.

Verification only reports. The one exception is --repair, which permanently
deletes storage objects no archived attachment points at, along with stale
temporary upload files. Nothing else is written, and no source is contacted:
this does not resynchronise anything from Discord.`

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	repair := flags.Bool("repair", false, "permanently delete untracked and stale temporary storage objects")
	if done, err := parseFlags(flags, verifyHelp, args); done {
		return err
	}
	if flags.NArg() > 1 {
		return errors.New(verifyUsage)
	}
	if flags.NArg() == 1 {
		if *repair {
			return errors.New("--repair applies only to the live archive")
		}
		manifest, err := preservation.VerifyExport(context.Background(), flags.Arg(0))
		if err != nil {
			return err
		}
		fmt.Printf("export verified: %d messages, %d attachments, %d blobs (format v%d)\n",
			manifest.Counts.Messages, manifest.Counts.Attachments, manifest.Counts.Blobs, manifest.FormatVersion)
		return nil
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
	blobs, err := openBlobStore(ctx, cfg)
	if err != nil {
		return err
	}
	report, err := preservation.VerifyLive(ctx, pool, blobs, *repair, time.Now().Add(-time.Hour))
	if err != nil {
		return err
	}
	fmt.Printf("live archive checked: %d messages, %d attachments, %d/%d blobs verified\n",
		report.Messages, report.Attachments, report.HashedBlobs, report.Blobs)
	if report.Removed > 0 {
		fmt.Printf("storage repaired: removed %d untracked or stale temporary objects\n", report.Removed)
	}
	for _, issue := range report.Issues {
		fmt.Fprintf(os.Stderr, "  - %s\n", issue)
	}
	if !report.Valid() {
		return errors.New("verification failed")
	}
	return nil
}
