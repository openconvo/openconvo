package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Asking a subcommand for help is a request, not a failure: flag.ErrHelp
// must not reach main, which would exit 1.
func TestSubcommandHelpIsNotAnError(t *testing.T) {
	discardStdout(t)
	commands := map[string]func([]string) error{
		"export":           runExport,
		"verify":           runVerify,
		"replay-deletions": runReplayDeletions,
		"mcp":              runMCP,
	}
	for name, run := range commands {
		for _, arg := range []string{"-h", "--help"} {
			if err := run([]string{arg}); err != nil {
				t.Errorf("%s %s = %v, want nil", name, arg, err)
			}
		}
	}
}

// discardStdout keeps the help text these commands print out of the test
// output.
func discardStdout(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = devnull
	t.Cleanup(func() {
		os.Stdout = stdout
		devnull.Close()
	})
}

// A bare deletion ledger is applied with no verification at all, so it
// must not be accepted by the path the usage line names first.
func TestRunReplayDeletionsRefusesUnverifiedLedgerFile(t *testing.T) {
	ledger := filepath.Join(t.TempDir(), "deletion_ledger.jsonl")
	if err := os.WriteFile(ledger, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runReplayDeletions([]string{ledger})
	if err == nil || !strings.Contains(err.Error(), "--unverified") {
		t.Fatalf("runReplayDeletions error = %v, want a refusal naming --unverified", err)
	}
}

// Confirmation fails closed: only an explicit yes proceeds, and end of
// input (an unattended run reading /dev/null) is a no.
func TestConfirmRequiresExplicitYes(t *testing.T) {
	cases := map[string]bool{
		"y\n":                 true,
		"yes\n":               true,
		"  YES  \n":           true,
		"n\n":                 false,
		"\n":                  false,
		"":                    false,
		"yes, do it\n":        false,
		"delete everything\n": false,
	}
	for answer, want := range cases {
		got, err := confirm(strings.NewReader(answer), io.Discard, "Apply this destructive operation?")
		if err != nil {
			t.Fatalf("confirm(%q) error = %v", answer, err)
		}
		if got != want {
			t.Errorf("confirm(%q) = %v, want %v", answer, got, want)
		}
	}
}
