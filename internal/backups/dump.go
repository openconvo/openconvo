package backups

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
)

type dumper interface {
	Dump(ctx context.Context, destination string) error
}

type pgDumper struct {
	binary      string
	databaseURL string
}

func (d pgDumper) Dump(ctx context.Context, destination string) error {
	dsn, password, err := pgDumpDSN(d.databaseURL)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, d.binary,
		"--format=custom",
		"--compress=6",
		"--no-owner",
		"--no-privileges",
		// Embedding values are derived and potentially very large. Preserve
		// their schema and generation provenance, but rebuild vectors from
		// canonical messages after a restore.
		"--exclude-table-data=derived.message_embeddings",
		"--file="+destination,
		"--dbname="+dsn,
	)
	cmd.Env = pgDumpEnvironment(password)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{dst: &stderr, remaining: 32 << 10}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if stderr.Len() > 0 {
			return fmt.Errorf("pg_dump: %s", bytes.TrimSpace(stderr.Bytes()))
		}
		return fmt.Errorf("pg_dump: %w", err)
	}
	return nil
}

// pgDumpEnvironment intentionally does not inherit OpenConvo's full
// environment. In particular, the dump subprocess has no reason to receive
// Discord or object-storage credentials.
func pgDumpEnvironment(password string) []string {
	environment := make([]string, 0, 8)
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ", "SYSTEMROOT"} {
		if value, ok := os.LookupEnv(name); ok {
			environment = append(environment, name+"="+value)
		}
	}
	if password != "" {
		environment = append(environment, "PGPASSWORD="+password)
	}
	return environment
}

// pgDumpDSN removes the password from the command line and returns it for the
// child process environment. libpq accepts a password in two places in a URL:
// the userinfo section and a password connection parameter. Both are stripped,
// because whatever is left in the DSN is passed as --dbname= and is readable in
// the process arguments for the life of the dump. DATABASE_URL is documented as
// a PostgreSQL URL, so rejecting keyword-form DSNs here avoids accidentally
// exposing a secret in process arguments.
func pgDumpDSN(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Host == "" {
		return "", "", fmt.Errorf("database backup requires DATABASE_URL in postgres:// URL form")
	}
	password, _ := u.User.Password()
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	if parameters := u.Query(); parameters.Has("password") {
		// libpq applies connection parameters after the userinfo section,
		// so a password parameter is the one that would take effect.
		password = parameters.Get("password")
		parameters.Del("password")
		u.RawQuery = parameters.Encode()
	}
	return u.String(), password, nil
}

type limitedWriter struct {
	dst       *bytes.Buffer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if w.remaining > 0 {
		keep := len(p)
		if keep > w.remaining {
			keep = w.remaining
		}
		_, _ = w.dst.Write(p[:keep])
		w.remaining -= keep
	}
	return n, nil
}
