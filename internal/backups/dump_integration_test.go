package backups

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/openconvo/openconvo/internal/testutil"
)

func TestPGDumperCreatesCustomFormatBackup(t *testing.T) {
	pool := testutil.NewDB(t)
	binary, err := exec.LookPath("pg_dump")
	if err != nil {
		t.Skip("pg_dump not installed on test host")
	}
	destination := filepath.Join(t.TempDir(), "openconvo.dump")
	dumper := pgDumper{binary: binary, databaseURL: pool.Config().ConnString()}
	if err := dumper.Dump(context.Background(), destination); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 5 || string(body[:5]) != "PGDMP" {
		t.Fatalf("dump does not have PostgreSQL custom-format header: %q", body[:min(len(body), 16)])
	}
}
