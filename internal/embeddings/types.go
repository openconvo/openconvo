// Package embeddings builds disposable semantic-search indexes from the
// canonical archive. It never owns message content and provider failures must
// never block archival writes.
package embeddings

import (
	"errors"
	"fmt"
	"strings"
)

const (
	ProviderOpenAI = "openai"
	ModelSmall     = "text-embedding-3-small"
	Dimensions     = 256
	InputVersion   = "message-content-v1"

	settingsKey = "embeddings"

	JobMessage = "embedding.message"
	JobSweep   = "embedding.sweep"
)

var (
	ErrInvalidSettings = errors.New("invalid embedding settings")
	ErrNotConfigured   = errors.New("embedding provider is not configured")
	ErrDisabled        = errors.New("message embeddings are disabled")
	ErrNotReady        = errors.New("message embedding index is not ready")
	ErrProvider        = errors.New("embedding provider request failed")
)

// Settings is deliberately a single preset for the first implementation. The
// identity fields are persisted and returned so every derived vector has clear
// provenance; adding arbitrary models waits for a real second preset/provider.
type Settings struct {
	Enabled      bool   `json:"enabled"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Dimensions   int    `json:"dimensions"`
	InputVersion string `json:"input_version"`
}

// SettingsView adds secret-free operational state for the administrator UI.
type SettingsView struct {
	Settings
	CredentialsConfigured bool   `json:"credentials_configured"`
	Source                string `json:"source"`
	GenerationStatus      string `json:"generation_status,omitempty"`
	EmbeddedMessages      int64  `json:"embedded_messages"`
	EligibleMessages      int64  `json:"eligible_messages"`
}

func Preset(enabled bool) Settings {
	return Settings{
		Enabled:      enabled,
		Provider:     ProviderOpenAI,
		Model:        ModelSmall,
		Dimensions:   Dimensions,
		InputVersion: InputVersion,
	}
}

func normalizeSettings(in Settings) (Settings, error) {
	in.Provider = strings.ToLower(strings.TrimSpace(in.Provider))
	in.Model = strings.TrimSpace(in.Model)
	in.InputVersion = strings.TrimSpace(in.InputVersion)
	if in.Provider == "" {
		in.Provider = ProviderOpenAI
	}
	if in.Model == "" {
		in.Model = ModelSmall
	}
	if in.Dimensions == 0 {
		in.Dimensions = Dimensions
	}
	if in.InputVersion == "" {
		in.InputVersion = InputVersion
	}
	if in.Provider != ProviderOpenAI || in.Model != ModelSmall || in.Dimensions != Dimensions || in.InputVersion != InputVersion {
		return Settings{}, fmt.Errorf("%w: the supported preset is %s/%s at %d dimensions with %s",
			ErrInvalidSettings, ProviderOpenAI, ModelSmall, Dimensions, InputVersion)
	}
	return in, nil
}
