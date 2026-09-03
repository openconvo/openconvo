// Package config loads OpenConvo configuration from environment
// variables. Configuration is deliberately boring: flat environment
// variables, documented in .env.example, no config file formats.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
)

// Storage driver names.
const (
	StorageDriverFilesystem = "filesystem"
	StorageDriverS3         = "s3"
)

// Config is the full application configuration.
type Config struct {
	// Host is the interface the HTTP server binds to. Empty means all
	// interfaces.
	Host string
	// Port is the HTTP listen port.
	Port int
	// AdminPassword is the single built-in administrator credential. It is
	// configuration, never archive data.
	AdminPassword string

	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// AutoMigrate applies pending schema migrations on startup.
	AutoMigrate bool

	// DiscordToken is the Discord bot token. Optional at startup: the
	// server boots without it and archival stays idle until configured.
	DiscordToken string
	// DiscordApplicationID is the Discord application ID.
	DiscordApplicationID string

	// StorageDriver selects the attachment blob store implementation.
	StorageDriver string
	// StoragePath is the filesystem storage root (filesystem driver).
	StoragePath string

	// S3 settings (s3 driver only).
	S3Endpoint       string
	S3Region         string
	S3Bucket         string
	S3AccessKey      string
	S3SecretKey      string
	S3SessionToken   string
	S3ForcePathStyle bool

	// AttachmentsEnabled turns on downloading the files attached to
	// archived messages. Off by default: an archive of any size is a
	// large, open-ended disk commitment, and that is the operator's
	// decision to make knowingly.
	AttachmentsEnabled bool
	// AttachmentMaxBytes is the per-file download ceiling.
	AttachmentMaxBytes int64

	// Database backup destination. Non-secret values are defaults that the
	// administrator may override from the dashboard. Credentials are always
	// environment-only: settings is explicitly not a secret store.
	BackupEnabled          bool
	BackupProvider         string
	BackupS3Endpoint       string
	BackupS3Region         string
	BackupS3Bucket         string
	BackupS3Prefix         string
	BackupS3AccessKey      string
	BackupS3SecretKey      string
	BackupS3SessionToken   string
	BackupS3ForcePathStyle bool
	BackupIntervalHours    int
	BackupRetentionCount   int
	BackupPGDumpPath       string

	// Embeddings are an optional derived index. Enabling is a separate,
	// explicit consent to send archived message text to OpenAI. The API key is
	// environment-only and never persisted in application settings.
	EmbeddingsEnabled bool
	OpenAIAPIKey      string

	// MCPHTTPEnabled exposes the read-only MCP search server at /mcp on the
	// existing HTTP listener. It is opt-in because this is a second way to read
	// private archive content from the network.
	MCPHTTPEnabled bool
	// MCPToken is the dedicated bearer credential for the remote MCP endpoint.
	// It remains environment-only and separate from browser authentication.
	MCPToken string

	// LogLevel is one of debug, info, warn, error.
	LogLevel slog.Level
	// LogFormat is "text" or "json".
	LogFormat string
}

// Load reads configuration from the process environment and validates it.
func Load() (Config, error) {
	cfg := Config{
		Host:                 getenv("OPENCONVO_HOST", ""),
		AdminPassword:        getenv("OPENCONVO_ADMIN_PASSWORD", ""),
		DatabaseURL:          getenv("DATABASE_URL", ""),
		DiscordToken:         getenv("DISCORD_TOKEN", ""),
		DiscordApplicationID: getenv("DISCORD_APPLICATION_ID", ""),
		StorageDriver:        getenv("STORAGE_DRIVER", StorageDriverFilesystem),
		StoragePath:          getenv("STORAGE_PATH", "/data/attachments"),
		S3Endpoint:           getenv("S3_ENDPOINT", ""),
		S3Region:             getenv("S3_REGION", ""),
		S3Bucket:             getenv("S3_BUCKET", ""),
		S3AccessKey:          getenv("S3_ACCESS_KEY", ""),
		S3SecretKey:          getenv("S3_SECRET_KEY", ""),
		S3SessionToken:       getenv("S3_SESSION_TOKEN", ""),
		BackupProvider:       getenv("BACKUP_PROVIDER", "r2"),
		BackupS3Endpoint:     getenv("BACKUP_S3_ENDPOINT", ""),
		BackupS3Region:       getenv("BACKUP_S3_REGION", "auto"),
		BackupS3Bucket:       getenv("BACKUP_S3_BUCKET", ""),
		BackupS3Prefix:       getenv("BACKUP_S3_PREFIX", "openconvo/database-backups"),
		BackupS3AccessKey:    getenv("BACKUP_S3_ACCESS_KEY", ""),
		BackupS3SecretKey:    getenv("BACKUP_S3_SECRET_KEY", ""),
		BackupS3SessionToken: getenv("BACKUP_S3_SESSION_TOKEN", ""),
		BackupPGDumpPath:     getenv("BACKUP_PG_DUMP_PATH", "pg_dump"),
		OpenAIAPIKey:         getenv("OPENAI_API_KEY", ""),
		MCPToken:             getenv("OPENCONVO_MCP_TOKEN", ""),
		LogFormat:            getenv("LOG_FORMAT", "text"),
	}

	port, err := parsePort(getenv("OPENCONVO_PORT", "8080"))
	if err != nil {
		return Config{}, err
	}
	cfg.Port = port

	autoMigrate, err := parseBool("OPENCONVO_AUTO_MIGRATE", getenv("OPENCONVO_AUTO_MIGRATE", "true"))
	if err != nil {
		return Config{}, err
	}
	cfg.AutoMigrate = autoMigrate

	attachmentsEnabled, err := parseBool("OPENCONVO_ATTACHMENTS_ENABLED",
		getenv("OPENCONVO_ATTACHMENTS_ENABLED", "false"))
	if err != nil {
		return Config{}, err
	}
	cfg.AttachmentsEnabled = attachmentsEnabled

	backupEnabled, err := parseBool("BACKUP_ENABLED", getenv("BACKUP_ENABLED", "false"))
	if err != nil {
		return Config{}, err
	}
	cfg.BackupEnabled = backupEnabled

	embeddingsEnabled, err := parseBool("OPENCONVO_EMBEDDINGS_ENABLED",
		getenv("OPENCONVO_EMBEDDINGS_ENABLED", "false"))
	if err != nil {
		return Config{}, err
	}
	cfg.EmbeddingsEnabled = embeddingsEnabled

	mcpHTTPEnabled, err := parseBool("OPENCONVO_MCP_HTTP_ENABLED",
		getenv("OPENCONVO_MCP_HTTP_ENABLED", "false"))
	if err != nil {
		return Config{}, err
	}
	cfg.MCPHTTPEnabled = mcpHTTPEnabled

	forcePathStyle, err := parseBool("S3_FORCE_PATH_STYLE",
		getenv("S3_FORCE_PATH_STYLE", "false"))
	if err != nil {
		return Config{}, err
	}
	cfg.S3ForcePathStyle = forcePathStyle

	backupForcePathStyle, err := parseBool("BACKUP_S3_FORCE_PATH_STYLE",
		getenv("BACKUP_S3_FORCE_PATH_STYLE", "false"))
	if err != nil {
		return Config{}, err
	}
	cfg.BackupS3ForcePathStyle = backupForcePathStyle

	backupInterval, err := parseIntRange("BACKUP_INTERVAL_HOURS",
		getenv("BACKUP_INTERVAL_HOURS", "24"), 1, 24*31)
	if err != nil {
		return Config{}, err
	}
	cfg.BackupIntervalHours = backupInterval

	backupRetention, err := parseIntRange("BACKUP_RETENTION_COUNT",
		getenv("BACKUP_RETENTION_COUNT", "30"), 1, 1000)
	if err != nil {
		return Config{}, err
	}
	cfg.BackupRetentionCount = backupRetention

	maxBytes, err := parseBytes("OPENCONVO_ATTACHMENT_MAX_BYTES",
		getenv("OPENCONVO_ATTACHMENT_MAX_BYTES", "104857600"))
	if err != nil {
		return Config{}, err
	}
	cfg.AttachmentMaxBytes = maxBytes

	level, err := parseLogLevel(getenv("LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if cfg.LogFormat != "text" && cfg.LogFormat != "json" {
		return Config{}, fmt.Errorf("LOG_FORMAT must be \"text\" or \"json\", got %q", cfg.LogFormat)
	}
	if cfg.BackupPGDumpPath == "" {
		return Config{}, fmt.Errorf("BACKUP_PG_DUMP_PATH must not be empty")
	}
	if cfg.EmbeddingsEnabled && cfg.OpenAIAPIKey == "" {
		return Config{}, fmt.Errorf("OPENCONVO_EMBEDDINGS_ENABLED=true requires OPENAI_API_KEY")
	}
	if cfg.MCPHTTPEnabled && len(cfg.MCPToken) < 32 {
		return Config{}, fmt.Errorf("OPENCONVO_MCP_HTTP_ENABLED=true requires OPENCONVO_MCP_TOKEN with at least 32 characters")
	}

	if err := validateStorage(&cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// RequireDatabase returns an error if no database is configured.
// Commands that need PostgreSQL call this explicitly so that commands
// like "openconvo version" work without any configuration.
func (c Config) RequireDatabase() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL is not set; see .env.example for configuration")
	}
	return nil
}

// RequireAdminPassword fails closed before the HTTP server starts. Commands
// that do not expose the archive can still run without this setting.
func (c Config) RequireAdminPassword() error {
	if len(c.AdminPassword) < 12 {
		return fmt.Errorf("OPENCONVO_ADMIN_PASSWORD must be set to at least 12 characters")
	}
	return nil
}

// BindWarning reports a bind address that is legal but almost certainly a
// mistake. Docker publishes a port into the container's own network
// namespace, so a server bound to loopback inside one is unreachable from
// that published port: the deployment starts, reports itself healthy from
// within, and answers nothing. A bare-process install behind a proxy on the
// same machine should bind loopback, so it is the container that makes this
// wrong rather than the address.
func (c Config) BindWarning(inContainer bool) string {
	if !inContainer || c.Host == "" {
		return ""
	}
	if ip := net.ParseIP(c.Host); ip == nil || !ip.IsLoopback() {
		return ""
	}
	return fmt.Sprintf("OPENCONVO_HOST=%s binds OpenConvo to loopback inside the container, "+
		"where the published port cannot reach it; leave OPENCONVO_HOST empty under docker compose",
		c.Host)
}

// DiscordConfigured reports whether a Discord bot token is present.
func (c Config) DiscordConfigured() bool {
	return strings.TrimSpace(c.DiscordToken) != ""
}

func validateStorage(cfg *Config) error {
	switch cfg.StorageDriver {
	case StorageDriverFilesystem:
		if cfg.StoragePath == "" {
			return fmt.Errorf("STORAGE_PATH must be set when STORAGE_DRIVER=filesystem")
		}
	case StorageDriverS3:
		var missing []string
		if cfg.S3Region == "" {
			missing = append(missing, "S3_REGION")
		}
		if cfg.S3Bucket == "" {
			missing = append(missing, "S3_BUCKET")
		}
		if cfg.S3AccessKey == "" {
			missing = append(missing, "S3_ACCESS_KEY")
		}
		if cfg.S3SecretKey == "" {
			missing = append(missing, "S3_SECRET_KEY")
		}
		if len(missing) > 0 {
			return fmt.Errorf("STORAGE_DRIVER=s3 requires %s", strings.Join(missing, ", "))
		}
	default:
		return fmt.Errorf("unknown STORAGE_DRIVER %q (expected %q or %q)",
			cfg.StorageDriver, StorageDriverFilesystem, StorageDriverS3)
	}
	return nil
}

// getenv reads a variable, trimming surrounding whitespace. The trim is
// deliberate: .env files routinely carry a stray trailing space, and a
// value nobody can see is a miserable thing to debug. It applies to
// secrets too, so a password cannot begin or end with whitespace.
func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(v)
	}
	return fallback
}

func parsePort(raw string) (int, error) {
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, fmt.Errorf("OPENCONVO_PORT must be a port number between 1 and 65535, got %q", raw)
	}
	return port, nil
}

func parseBool(name, raw string) (bool, error) {
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean, got %q", name, raw)
	}
	return v, nil
}

func parseBytes(name, raw string) (int64, error) {
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of bytes, got %q", name, raw)
	}
	return v, nil
}

func parseIntRange(name, raw string, minValue, maxValue int) (int, error) {
	v, err := strconv.Atoi(raw)
	if err != nil || v < minValue || v > maxValue {
		return 0, fmt.Errorf("%s must be between %d and %d, got %q", name, minValue, maxValue, raw)
	}
	return v, nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, error; got %q", raw)
	}
}
