package config

import (
	"log/slog"
	"strings"
	"testing"
)

// setEnv sets environment variables for the duration of a test.
func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{
		// t.Setenv with empty values ensures host env vars don't leak in.
		"OPENCONVO_HOST": "", "OPENCONVO_PORT": "", "OPENCONVO_ADMIN_PASSWORD": "",
		"DATABASE_URL": "", "DISCORD_TOKEN": "", "STORAGE_DRIVER": "",
		"STORAGE_PATH": "", "LOG_LEVEL": "", "LOG_FORMAT": "",
		"OPENCONVO_AUTO_MIGRATE": "", "S3_ENDPOINT": "", "S3_REGION": "",
		"S3_BUCKET": "", "S3_ACCESS_KEY": "", "S3_SECRET_KEY": "",
		"S3_SESSION_TOKEN": "", "S3_FORCE_PATH_STYLE": "",
		"BACKUP_ENABLED": "false", "BACKUP_PROVIDER": "r2",
		"BACKUP_S3_ENDPOINT": "", "BACKUP_S3_REGION": "auto",
		"BACKUP_S3_BUCKET": "", "BACKUP_S3_PREFIX": "openconvo/database-backups",
		"BACKUP_S3_ACCESS_KEY": "", "BACKUP_S3_SECRET_KEY": "",
		"BACKUP_S3_SESSION_TOKEN": "", "BACKUP_S3_FORCE_PATH_STYLE": "false",
		"BACKUP_INTERVAL_HOURS": "24", "BACKUP_RETENTION_COUNT": "30",
		"BACKUP_PG_DUMP_PATH":          "pg_dump",
		"OPENCONVO_EMBEDDINGS_ENABLED": "false", "OPENAI_API_KEY": "",
		"OPENCONVO_MCP_HTTP_ENABLED": "false", "OPENCONVO_MCP_TOKEN": "",
	})
	// Empty strings are still "set", so point the ones with defaults at
	// their expected default explicitly where empty is invalid.
	t.Setenv("OPENCONVO_PORT", "8080")
	t.Setenv("STORAGE_DRIVER", "filesystem")
	t.Setenv("STORAGE_PATH", "/data/attachments")
	t.Setenv("LOG_LEVEL", "info")
	t.Setenv("LOG_FORMAT", "text")
	t.Setenv("OPENCONVO_AUTO_MIGRATE", "true")
	t.Setenv("S3_FORCE_PATH_STYLE", "false")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
	if cfg.StorageDriver != StorageDriverFilesystem {
		t.Errorf("StorageDriver = %q, want filesystem", cfg.StorageDriver)
	}
	if !cfg.AutoMigrate {
		t.Error("AutoMigrate = false, want true by default")
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.DiscordConfigured() {
		t.Error("DiscordConfigured() = true with empty token")
	}
	if cfg.MCPHTTPEnabled {
		t.Error("remote MCP enabled by default")
	}
	if err := cfg.RequireDatabase(); err == nil {
		t.Error("RequireDatabase() = nil with empty DATABASE_URL, want error")
	}
	if err := cfg.RequireAdminPassword(); err == nil {
		t.Error("RequireAdminPassword() = nil with empty password, want error")
	}
}

func TestRemoteMCPRequiresDedicatedLongToken(t *testing.T) {
	t.Setenv("OPENCONVO_MCP_HTTP_ENABLED", "true")
	t.Setenv("OPENCONVO_MCP_TOKEN", "")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OPENCONVO_MCP_TOKEN") {
		t.Fatalf("missing token error = %v", err)
	}

	t.Setenv("OPENCONVO_MCP_TOKEN", "too-short")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at least 32") {
		t.Fatalf("short token error = %v", err)
	}

	token := strings.Repeat("a", 64)
	t.Setenv("OPENCONVO_MCP_TOKEN", "  "+token+"  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.MCPHTTPEnabled || cfg.MCPToken != token {
		t.Fatalf("remote MCP config = enabled:%v token:%q", cfg.MCPHTTPEnabled, cfg.MCPToken)
	}

	t.Setenv("OPENCONVO_MCP_HTTP_ENABLED", "sometimes")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "OPENCONVO_MCP_HTTP_ENABLED") {
		t.Fatalf("invalid enabled error = %v", err)
	}
}

func TestRequireAdminPassword(t *testing.T) {
	t.Setenv("OPENCONVO_ADMIN_PASSWORD", "correct horse battery staple")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.RequireAdminPassword(); err != nil {
		t.Fatalf("RequireAdminPassword: %v", err)
	}

	cfg.AdminPassword = "too-short"
	if err := cfg.RequireAdminPassword(); err == nil {
		t.Fatal("RequireAdminPassword accepted a short password")
	}
}

func TestLoadInvalidPort(t *testing.T) {
	t.Setenv("OPENCONVO_PORT", "notaport")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error with invalid port, want error")
	}
	t.Setenv("OPENCONVO_PORT", "99999")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error with out-of-range port, want error")
	}
}

func TestLoadHost(t *testing.T) {
	t.Setenv("OPENCONVO_HOST", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "" {
		t.Errorf("Host = %q, want empty (every interface) by default", cfg.Host)
	}

	t.Setenv("OPENCONVO_HOST", "127.0.0.1")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want 127.0.0.1", cfg.Host)
	}
}

func TestLoadAutoMigrate(t *testing.T) {
	// An operator holding migrations back before a risky upgrade must get
	// exactly that, so compose forwards the variable and Load honours it.
	t.Setenv("OPENCONVO_AUTO_MIGRATE", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoMigrate {
		t.Error("AutoMigrate = true with OPENCONVO_AUTO_MIGRATE=false")
	}

	t.Setenv("OPENCONVO_AUTO_MIGRATE", "maybe")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted an invalid OPENCONVO_AUTO_MIGRATE")
	}
}

func TestLoadTrimsSurroundingWhitespace(t *testing.T) {
	// Trimming is deliberate and applies to secrets too: a .env file
	// picking up a trailing space must not change the value.
	t.Setenv("OPENCONVO_HOST", " 127.0.0.1 ")
	t.Setenv("OPENCONVO_ADMIN_PASSWORD", "  correct horse battery staple  ")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want the trimmed value", cfg.Host)
	}
	if cfg.AdminPassword != "correct horse battery staple" {
		t.Errorf("AdminPassword = %q, want the trimmed value", cfg.AdminPassword)
	}
}

func TestLoadS3RequiresCredentials(t *testing.T) {
	t.Setenv("STORAGE_DRIVER", "s3")
	t.Setenv("S3_BUCKET", "")
	t.Setenv("S3_REGION", "")
	t.Setenv("S3_ACCESS_KEY", "")
	t.Setenv("S3_SECRET_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error for s3 driver without credentials, want error")
	}

	t.Setenv("S3_BUCKET", "archive")
	t.Setenv("S3_REGION", "auto")
	t.Setenv("S3_ACCESS_KEY", "key")
	t.Setenv("S3_SECRET_KEY", "secret")
	t.Setenv("S3_SESSION_TOKEN", "temporary-token")
	t.Setenv("S3_FORCE_PATH_STYLE", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error with full s3 config: %v", err)
	}
	if cfg.StorageDriver != StorageDriverS3 {
		t.Errorf("StorageDriver = %q, want s3", cfg.StorageDriver)
	}
	if cfg.S3Region != "auto" || cfg.S3SessionToken != "temporary-token" || !cfg.S3ForcePathStyle {
		t.Errorf("S3 config = %+v", cfg)
	}
}

func TestLoadInvalidS3PathStyle(t *testing.T) {
	t.Setenv("S3_FORCE_PATH_STYLE", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted invalid S3_FORCE_PATH_STYLE")
	}
}

func TestLoadUnknownStorageDriver(t *testing.T) {
	t.Setenv("STORAGE_DRIVER", "floppy")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error with unknown storage driver, want error")
	}
}

func TestLoadLogLevels(t *testing.T) {
	cases := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for raw, want := range cases {
		t.Setenv("LOG_LEVEL", raw)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load() error for LOG_LEVEL=%s: %v", raw, err)
		}
		if cfg.LogLevel != want {
			t.Errorf("LogLevel for %q = %v, want %v", raw, cfg.LogLevel, want)
		}
	}
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error with invalid log level, want error")
	}
}

func TestAttachmentSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://localhost/x")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AttachmentsEnabled {
		t.Error("attachments enabled by default; downloading gigabytes must be opt-in")
	}
	if cfg.AttachmentMaxBytes != 100<<20 {
		t.Errorf("AttachmentMaxBytes = %d, want %d", cfg.AttachmentMaxBytes, 100<<20)
	}

	t.Setenv("OPENCONVO_ATTACHMENTS_ENABLED", "true")
	t.Setenv("OPENCONVO_ATTACHMENT_MAX_BYTES", "1048576")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AttachmentsEnabled || cfg.AttachmentMaxBytes != 1048576 {
		t.Errorf("cfg = %+v", cfg)
	}

	t.Setenv("OPENCONVO_ATTACHMENT_MAX_BYTES", "not-a-number")
	if _, err := Load(); err == nil {
		t.Error("a non-numeric size was accepted")
	}
}

func TestDiscordConfigured(t *testing.T) {
	t.Setenv("DISCORD_TOKEN", "abc123")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.DiscordConfigured() {
		t.Error("DiscordConfigured() = false with token set")
	}
}

func TestBackupSettings(t *testing.T) {
	t.Setenv("BACKUP_ENABLED", "true")
	t.Setenv("BACKUP_PROVIDER", "r2")
	t.Setenv("BACKUP_S3_ENDPOINT", "https://account.r2.cloudflarestorage.com")
	t.Setenv("BACKUP_S3_REGION", "auto")
	t.Setenv("BACKUP_S3_BUCKET", "openconvo-backups")
	t.Setenv("BACKUP_S3_PREFIX", "db")
	t.Setenv("BACKUP_S3_ACCESS_KEY", "access")
	t.Setenv("BACKUP_S3_SECRET_KEY", "secret")
	t.Setenv("BACKUP_S3_FORCE_PATH_STYLE", "false")
	t.Setenv("BACKUP_INTERVAL_HOURS", "12")
	t.Setenv("BACKUP_RETENTION_COUNT", "14")
	t.Setenv("BACKUP_PG_DUMP_PATH", "/usr/bin/pg_dump")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BackupEnabled || cfg.BackupProvider != "r2" || cfg.BackupIntervalHours != 12 || cfg.BackupRetentionCount != 14 {
		t.Errorf("backup config = %+v", cfg)
	}
	if cfg.BackupS3SecretKey != "secret" || cfg.BackupPGDumpPath != "/usr/bin/pg_dump" {
		t.Errorf("backup secrets/tool config = %+v", cfg)
	}

	t.Setenv("BACKUP_INTERVAL_HOURS", "0")
	if _, err := Load(); err == nil {
		t.Error("BACKUP_INTERVAL_HOURS=0 was accepted")
	}
	t.Setenv("BACKUP_INTERVAL_HOURS", "24")
	t.Setenv("BACKUP_RETENTION_COUNT", "1001")
	if _, err := Load(); err == nil {
		t.Error("BACKUP_RETENTION_COUNT=1001 was accepted")
	}
}

func TestEmbeddingSettings(t *testing.T) {
	t.Setenv("OPENCONVO_EMBEDDINGS_ENABLED", "false")
	t.Setenv("OPENAI_API_KEY", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbeddingsEnabled {
		t.Fatal("embeddings enabled by default; external processing must be opt-in")
	}

	t.Setenv("OPENCONVO_EMBEDDINGS_ENABLED", "true")
	if _, err := Load(); err == nil {
		t.Fatal("enabled embeddings accepted without OPENAI_API_KEY")
	}
	t.Setenv("OPENAI_API_KEY", "secret")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.EmbeddingsEnabled || cfg.OpenAIAPIKey != "secret" {
		t.Errorf("embedding config = %+v", cfg)
	}

	t.Setenv("OPENCONVO_EMBEDDINGS_ENABLED", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("invalid OPENCONVO_EMBEDDINGS_ENABLED accepted")
	}
}

func TestBindWarning(t *testing.T) {
	cases := []struct {
		name        string
		host        string
		inContainer bool
		warn        bool
	}{
		{name: "compose default binds every interface", host: "", inContainer: true},
		{name: "loopback in a container is unreachable", host: "127.0.0.1", inContainer: true, warn: true},
		{name: "IPv6 loopback in a container is unreachable", host: "::1", inContainer: true, warn: true},
		{name: "loopback outside a container is the recommendation", host: "127.0.0.1"},
		{name: "an explicit interface in a container is deliberate", host: "0.0.0.0", inContainer: true},
		{name: "a private address in a container is deliberate", host: "10.0.0.5", inContainer: true},
		{name: "a hostname is not something to guess about", host: "openconvo.internal", inContainer: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			warning := Config{Host: tc.host}.BindWarning(tc.inContainer)
			if tc.warn && warning == "" {
				t.Fatalf("OPENCONVO_HOST=%q in a container produced no warning", tc.host)
			}
			if !tc.warn && warning != "" {
				t.Fatalf("OPENCONVO_HOST=%q warned needlessly: %s", tc.host, warning)
			}
		})
	}
}
