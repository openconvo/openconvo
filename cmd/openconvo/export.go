package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/database"
	"github.com/openconvo/openconvo/internal/preservation"
	"github.com/openconvo/openconvo/internal/version"
)

const exportUsage = `usage: openconvo export [--format jsonl|markdown] [--output|-o] <directory>`

const exportHelp = exportUsage + `

Writes a portable copy of the archive — messages, attachments, blobs and the
deletion ledger — to a new directory, with a manifest "openconvo verify"
can check it against. The destination may be given as --output or as the
single positional argument.`

func runExport(args []string) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	output := flags.String("output", "", "destination directory (must not already exist)")
	flags.StringVar(output, "o", "", "destination directory (shorthand)")
	format := flags.String("format", "jsonl", "export format: jsonl or markdown")
	if done, err := parseFlags(flags, exportHelp, args); done {
		return err
	}
	if flags.NArg() > 1 || (*output != "" && flags.NArg() == 1) {
		return errors.New(exportUsage)
	}
	if *output == "" && flags.NArg() == 1 {
		*output = flags.Arg(0)
	}
	if *output == "" {
		return errors.New(exportUsage)
	}
	if *format != "jsonl" && *format != "markdown" {
		return fmt.Errorf("unsupported export format %q (choose jsonl or markdown)", *format)
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
	manifest, err := preservation.Export(ctx, preservation.ExportOptions{
		Pool: pool, Blobs: blobs, Destination: *output,
		OpenConvoVersion: version.Version, RenderMarkdown: *format == "markdown",
	})
	if err != nil {
		return err
	}
	fmt.Printf("exported %d messages, %d attachments, and %d blobs to %s",
		manifest.Counts.Messages, manifest.Counts.Attachments, manifest.Counts.Blobs, *output)
	if *format == "markdown" {
		fmt.Print(" with Markdown rendering")
	}
	fmt.Println()
	return nil
}
