// Command openconvo is the single OpenConvo binary: server, CLI and
// operational tooling in one executable, run directly or inside the
// official Docker image (docker compose exec openconvo openconvo ...).
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/openconvo/openconvo/internal/config"
	"github.com/openconvo/openconvo/internal/version"
)

const usage = `OpenConvo — your community's knowledge, kept.

Usage:
  openconvo <command> [flags]

Commands:
  serve             Run the OpenConvo server (API, frontend, sync, background jobs)
  mcp               Expose read-only archive search to an MCP client over stdio
  migrate           Apply pending database migrations and exit
  status            Show archive status
  export            Write a portable archive, optionally rendered as Markdown
  verify            Check an export, or the live archive, against its own hashes
  replay-deletions  Reapply a deletion ledger after restoring a database backup
  healthcheck       Probe the running server's /health endpoint (used by Docker)
  version           Print version information
  help              Show this help

Command flags:
  export            [--format jsonl|markdown] [--output|-o] <directory>
  verify            [--repair] [<export-directory>]
  replay-deletions  [--unverified] [--yes] <export-directory|deletion_ledger.jsonl>

Run "openconvo <command> --help" for one command's full flags.

Destructive — these erase archived data and cannot be undone without
restoring a database backup:
  replay-deletions  reapplies a ledger: tombstones the messages it names and
                    deletes the attachments, channels and communities it names
  verify --repair   permanently deletes untracked and stale temporary files
                    from storage

Configuration is read from environment variables; see .env.example.
`

func main() {
	cmd := "help"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	var err error
	switch cmd {
	case "serve":
		err = runServe()
	case "mcp":
		err = runMCP(os.Args[2:])
	case "migrate":
		err = runMigrate()
	case "status":
		err = runStatus()
	case "export":
		err = runExport(os.Args[2:])
	case "verify":
		err = runVerify(os.Args[2:])
	case "replay-deletions":
		err = runReplayDeletions(os.Args[2:])
	case "healthcheck":
		err = runHealthcheck()
	case "version", "-v", "--version":
		fmt.Println(version.Get())
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "openconvo: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "openconvo %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

// parseFlags parses one subcommand's arguments. The flag package is kept
// silent so that a bad flag is reported exactly once — here as usage, and
// then by main as the error — and so that -h/--help prints the same usage
// the command shows on bad arguments rather than failing with flag.ErrHelp.
// done reports that the command has nothing left to do.
func parseFlags(flags *flag.FlagSet, help string, args []string) (done bool, err error) {
	flags.SetOutput(io.Discard)
	switch err = flags.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		printCommandHelp(os.Stdout, flags, help)
		return true, nil
	case err != nil:
		printCommandHelp(os.Stderr, flags, help)
		return true, err
	}
	return false, nil
}

// printCommandHelp writes a subcommand's usage text followed by its flags.
func printCommandHelp(w io.Writer, flags *flag.FlagSet, help string) {
	fmt.Fprintf(w, "%s\n\nFlags:\n", help)
	flags.SetOutput(w)
	flags.PrintDefaults()
	flags.SetOutput(io.Discard)
}

// errNotConfirmable reports that a destructive command has no terminal to
// ask for confirmation on. Unattended runs must fail here, not proceed.
var errNotConfirmable = errors.New("standard input is not a terminal, so this cannot be confirmed interactively; re-run with --yes to proceed without a prompt")

// stdinIsTerminal reports whether standard input is an interactive
// terminal, and so whether a confirmation prompt can be answered at all.
func stdinIsTerminal() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// confirmDestructive requires an explicit yes before data is destroyed.
// --yes skips the prompt for scripted use; with no terminal to ask on, it
// refuses rather than proceeding.
func confirmDestructive(question string, assumeYes bool) error {
	if assumeYes {
		return nil
	}
	if !stdinIsTerminal() {
		return errNotConfirmable
	}
	confirmed, err := confirm(os.Stdin, os.Stdout, question)
	if err != nil {
		return err
	}
	if !confirmed {
		return errors.New("aborted; nothing was changed")
	}
	return nil
}

// confirm asks a yes/no question. It fails closed: anything but "y" or
// "yes" — including end of input — means no. The answer is bounded so that
// input that never ends a line cannot hang the prompt.
func confirm(in io.Reader, out io.Writer, question string) (bool, error) {
	const maxAnswer = 4096
	fmt.Fprintf(out, "%s [y/N]: ", question)
	answer, err := bufio.NewReader(io.LimitReader(in, maxAnswer)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}

// newLogger builds the process logger from configuration.
func newLogger(cfg config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	var handler slog.Handler
	if cfg.LogFormat == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
