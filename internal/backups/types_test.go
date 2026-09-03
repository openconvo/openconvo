package backups

import (
	"strings"
	"testing"
	"time"
)

func TestNormalizeProviderSettings(t *testing.T) {
	r2, err := normalizeSettings(Settings{
		Provider: "R2", Endpoint: "https://example.r2.cloudflarestorage.com/", Region: "wrong",
		Bucket: "archive", Prefix: "/openconvo/db/", IntervalHours: 24, RetentionCount: 30,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Provider != "r2" || r2.Region != "auto" || r2.Endpoint != "https://example.r2.cloudflarestorage.com" || r2.Prefix != "openconvo/db" {
		t.Errorf("normalized R2 settings = %+v", r2)
	}

	aws, err := normalizeSettings(Settings{
		Provider: "s3", Endpoint: "https://ignored.invalid", Bucket: "archive",
		Prefix: "db", IntervalHours: 1, RetentionCount: 1,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if aws.Endpoint != "" || aws.Region != "us-east-1" {
		t.Errorf("normalized S3 settings = %+v", aws)
	}
}

func TestNormalizeSettingsRejectsUnsafeValues(t *testing.T) {
	base := Settings{
		Provider: "custom", Endpoint: "https://objects.example.com", Region: "test",
		Bucket: "archive", Prefix: "db", IntervalHours: 24, RetentionCount: 30,
	}
	for name, mutate := range map[string]func(*Settings){
		"endpoint credentials": func(s *Settings) { s.Endpoint = "https://user:pass@objects.example.com" },
		"insecure R2":          func(s *Settings) { s.Provider, s.Endpoint = "r2", "http://account.r2.cloudflarestorage.com" },
		"unsafe prefix":        func(s *Settings) { s.Prefix = "db/../secret" },
		"zero interval":        func(s *Settings) { s.IntervalHours = 0 },
		"unknown provider":     func(s *Settings) { s.Provider = "ftp" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			mutate(&input)
			if _, err := normalizeSettings(input, true); err == nil {
				t.Fatalf("settings accepted: %+v", input)
			}
		})
	}
}

func TestDisabledSettingsMayBeConfiguredIncrementally(t *testing.T) {
	settings, err := normalizeSettings(Settings{
		Provider: "r2", Bucket: "not-yet-complete", IntervalHours: 24, RetentionCount: 30,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Bucket != "not-yet-complete" {
		t.Errorf("settings = %+v", settings)
	}
}

func TestObjectKeyAndFilename(t *testing.T) {
	at := time.Date(2026, 8, 20, 3, 4, 5, 0, time.UTC)
	key := objectKey("openconvo/db", at, "abcdef")
	if key != "openconvo/db/openconvo-db-20260820T030405Z-abcdef.dump" {
		t.Errorf("key = %q", key)
	}
	backup := Backup{CreatedAt: at}
	if backup.Filename() != "openconvo-db-20260820T030405Z.dump" {
		t.Errorf("filename = %q", backup.Filename())
	}
}

func TestPgDumpDSNHidesPassword(t *testing.T) {
	// libpq reads the password from the userinfo section or from a password
	// connection parameter, and the parameter wins when both are present.
	// Neither may survive into the DSN that becomes a --dbname= argument.
	for name, raw := range map[string]string{
		"userinfo":        "postgres://openconvo:p%40ss@db:5432/archive?sslmode=require",
		"query parameter": "postgres://openconvo@db:5432/archive?password=p%40ss&sslmode=require",
		"both":            "postgres://openconvo:userinfo@db:5432/archive?password=p%40ss&sslmode=require",
	} {
		t.Run(name, func(t *testing.T) {
			dsn, password, err := pgDumpDSN(raw)
			if err != nil {
				t.Fatal(err)
			}
			if password != "p@ss" {
				t.Errorf("password = %q, want %q", password, "p@ss")
			}
			for _, secret := range []string{"p%40ss", "p@ss", "password", "userinfo"} {
				if strings.Contains(dsn, secret) {
					t.Errorf("dsn %q still carries %q", dsn, secret)
				}
			}
			if !strings.Contains(dsn, "sslmode=require") {
				t.Errorf("dsn lost parameters: %q", dsn)
			}
			if !strings.Contains(dsn, "openconvo@db:5432/archive") {
				t.Errorf("dsn lost connection details: %q", dsn)
			}
		})
	}
	if _, _, err := pgDumpDSN("host=db password=secret"); err == nil {
		t.Error("keyword DSN was accepted")
	}
}

func TestPgDumpEnvironmentDoesNotInheritApplicationSecrets(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "discord-secret")
	t.Setenv("BACKUP_S3_SECRET_KEY", "storage-secret")
	environment := strings.Join(pgDumpEnvironment("database-secret"), "\n")
	if strings.Contains(environment, "discord-secret") || strings.Contains(environment, "storage-secret") {
		t.Fatalf("application secret inherited by pg_dump: %s", environment)
	}
	if !strings.Contains(environment, "PGPASSWORD=database-secret") {
		t.Fatalf("database password missing from pg_dump environment: %s", environment)
	}
}
