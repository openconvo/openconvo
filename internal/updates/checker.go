// Package updates checks the project's published releases. It is deliberately
// read-only: applying an update remains a host operation so the OpenConvo
// container never needs access to the Docker daemon.
package updates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const defaultReleaseURL = "https://api.github.com/repos/openconvo/openconvo/releases/latest"

// Status is the administrator-facing result of a release check.
type Status struct {
	CurrentVersion        string    `json:"current_version"`
	LatestVersion         string    `json:"latest_version,omitempty"`
	UpdateAvailable       bool      `json:"update_available"`
	CommandUpgradeAllowed bool      `json:"command_upgrade_allowed"`
	Reason                string    `json:"reason"`
	ReleaseURL            string    `json:"release_url,omitempty"`
	PublishedAt           time.Time `json:"published_at,omitempty"`
	CheckedAt             time.Time `json:"checked_at"`
	UpgradeCommand        string    `json:"upgrade_command,omitempty"`
}

// Checker retrieves and caches the latest stable GitHub release.
type Checker struct {
	currentVersion string
	endpoint       string
	client         *http.Client
	cacheFor       time.Duration
	now            func() time.Time

	mu      sync.Mutex
	cached  Status
	err     error
	expires time.Time
}

// New returns a release checker suitable for the long-running server.
func New(currentVersion string) *Checker {
	return &Checker{
		currentVersion: currentVersion,
		endpoint:       defaultReleaseURL,
		client:         &http.Client{Timeout: 10 * time.Second},
		cacheFor:       6 * time.Hour,
		now:            time.Now,
	}
}

type githubRelease struct {
	TagName     string    `json:"tag_name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
}

// Check returns the newest stable release. Successes and failures are cached
// so a dashboard refresh cannot exhaust GitHub's unauthenticated API limit.
func (c *Checker) Check(ctx context.Context) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now().UTC()
	if now.Before(c.expires) {
		return c.cached, c.err
	}

	status, err := c.fetch(ctx, now)
	c.cached, c.err = status, err
	if err != nil {
		c.expires = now.Add(15 * time.Minute)
	} else {
		c.expires = now.Add(c.cacheFor)
	}
	return status, err
}

func (c *Checker) fetch(ctx context.Context, checkedAt time.Time) (Status, error) {
	status := Status{CurrentVersion: c.currentVersion, CheckedAt: checkedAt}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return status, fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "openconvo-update-check")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := c.client.Do(req)
	if err != nil {
		return status, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return status, fmt.Errorf("fetch latest release: HTTP %d", resp.StatusCode)
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return status, fmt.Errorf("decode latest release: %w", err)
	}
	if release.TagName == "" || release.HTMLURL == "" {
		return status, errors.New("latest release response is incomplete")
	}

	status.LatestVersion = strings.TrimPrefix(release.TagName, "v")
	status.ReleaseURL = release.HTMLURL
	status.PublishedAt = release.PublishedAt

	if isDevelopmentBuild(c.currentVersion) {
		status.Reason = "development-build"
		return status, nil
	}
	current, err := parseVersion(c.currentVersion)
	if err != nil {
		status.Reason = "development-build"
		return status, nil
	}
	latest, err := parseVersion(release.TagName)
	if err != nil {
		return status, fmt.Errorf("latest release has an invalid semantic version %q", release.TagName)
	}

	comparison := current.compare(latest)
	if comparison >= 0 {
		status.Reason = "up-to-date"
		return status, nil
	}

	status.UpdateAvailable = true
	if commandUpgradeCompatible(current, latest) {
		status.CommandUpgradeAllowed = true
		status.Reason = "update-available"
		status.UpgradeCommand = "./scripts/upgrade.sh " + status.LatestVersion
	} else {
		status.Reason = "manual-upgrade-required"
	}
	return status, nil
}

// Before 1.0, minor releases may contain breaking changes; after 1.0, a major
// version change is the compatibility boundary. Prerelease-to-stable upgrades
// use the numeric target version for this decision.
func commandUpgradeCompatible(current, latest semVersion) bool {
	if current.major != latest.major {
		return false
	}
	if current.major == 0 && current.minor != latest.minor {
		return false
	}
	return true
}
