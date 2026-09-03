// Package backups creates, schedules, stores, and serves logical PostgreSQL
// backups. PostgreSQL dump semantics stay delegated to pg_dump; this package
// owns OpenConvo policy and S3-compatible object storage.
package backups

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"
)

const (
	JobDatabaseBackup = "backup.database"
	settingsKey       = "database_backup"
)

var (
	ErrNotFound        = errors.New("backup not found")
	ErrNotConfigured   = errors.New("backup destination is not configured")
	ErrDestination     = errors.New("backup destination is not reachable")
	ErrInvalidSettings = errors.New("invalid backup settings")
)

// Settings contains only non-secret backup policy. S3 credentials are loaded
// from the environment and are deliberately absent from this type.
type Settings struct {
	Enabled        bool   `json:"enabled"`
	Provider       string `json:"provider"`
	Endpoint       string `json:"endpoint"`
	Region         string `json:"region"`
	Bucket         string `json:"bucket"`
	Prefix         string `json:"prefix"`
	ForcePathStyle bool   `json:"force_path_style"`
	IntervalHours  int    `json:"interval_hours"`
	RetentionCount int    `json:"retention_count"`
}

// SettingsView is the administrator-facing effective configuration.
type SettingsView struct {
	Settings
	CredentialsConfigured bool   `json:"credentials_configured"`
	Source                string `json:"source"`
}

// Credentials are environment-only S3 credentials.
type Credentials struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
}

func (c Credentials) Configured() bool {
	return strings.TrimSpace(c.AccessKey) != "" && strings.TrimSpace(c.SecretKey) != ""
}

// Backup is one logical dump and its immutable destination snapshot.
type Backup struct {
	ID                string     `json:"id"`
	Trigger           string     `json:"trigger"`
	Status            string     `json:"status"`
	Provider          string     `json:"provider"`
	Endpoint          string     `json:"endpoint,omitempty"`
	Region            string     `json:"region"`
	Bucket            string     `json:"bucket"`
	Prefix            string     `json:"prefix"`
	ForcePathStyle    bool       `json:"force_path_style"`
	RetentionCount    int        `json:"retention_count"`
	ObjectKey         string     `json:"object_key"`
	Size              int64      `json:"size"`
	SHA256            string     `json:"sha256,omitempty"`
	Error             string     `json:"error,omitempty"`
	StartedAt         *time.Time `json:"started_at,omitempty"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	DownloadAvailable bool       `json:"download_available"`
}

func normalizeSettings(in Settings, requireDestination bool) (Settings, error) {
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	in.Region = strings.TrimSpace(in.Region)
	in.Bucket = strings.TrimSpace(in.Bucket)
	in.Prefix = strings.Trim(strings.TrimSpace(in.Prefix), "/")

	switch in.Provider {
	case "s3":
		if in.Region == "" {
			in.Region = "us-east-1"
		}
		in.Endpoint = ""
		in.ForcePathStyle = false
	case "r2":
		in.Region = "auto"
		in.ForcePathStyle = false
	case "backblaze":
		in.ForcePathStyle = false
	case "custom":
	default:
		return Settings{}, fmt.Errorf("%w: provider must be s3, r2, backblaze, or custom", ErrInvalidSettings)
	}

	if in.IntervalHours < 1 || in.IntervalHours > 24*31 {
		return Settings{}, fmt.Errorf("%w: interval_hours must be between 1 and %d", ErrInvalidSettings, 24*31)
	}
	if in.RetentionCount < 1 || in.RetentionCount > 1000 {
		return Settings{}, fmt.Errorf("%w: retention_count must be between 1 and 1000", ErrInvalidSettings)
	}
	if in.Prefix == "" {
		in.Prefix = "openconvo/database-backups"
	}
	if len(in.Prefix) > 200 || hasUnsafePathPart(in.Prefix) {
		return Settings{}, fmt.Errorf("%w: prefix must be a safe object-key prefix of at most 200 characters", ErrInvalidSettings)
	}

	if !requireDestination {
		return in, nil
	}
	if in.Bucket == "" || len(in.Bucket) > 255 {
		return Settings{}, fmt.Errorf("%w: bucket is required", ErrInvalidSettings)
	}
	if in.Region == "" {
		return Settings{}, fmt.Errorf("%w: region is required", ErrInvalidSettings)
	}
	if in.Provider != "s3" {
		if err := validateEndpoint(in.Endpoint); err != nil {
			return Settings{}, err
		}
		if in.Provider != "custom" && strings.HasPrefix(in.Endpoint, "http://") {
			return Settings{}, fmt.Errorf("%w: %s endpoint must use https", ErrInvalidSettings, in.Provider)
		}
	}
	return in, nil
}

func validateEndpoint(raw string) error {
	if raw == "" {
		return fmt.Errorf("%w: endpoint is required", ErrInvalidSettings)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: endpoint must be an http or https origin without credentials, query, or fragment", ErrInvalidSettings)
	}
	return nil
}

func hasUnsafePathPart(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return true
		}
	}
	return false
}

func objectKey(prefix string, createdAt time.Time, suffix string) string {
	name := "openconvo-db-" + createdAt.UTC().Format("20060102T150405Z") + "-" + suffix + ".dump"
	return path.Join(prefix, name)
}

// Filename is the safe browser download name for a backup.
func (b Backup) Filename() string {
	return "openconvo-db-" + b.CreatedAt.UTC().Format("20060102T150405Z") + ".dump"
}
