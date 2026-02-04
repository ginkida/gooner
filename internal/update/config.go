package update

import (
	"time"
)

// Config holds configuration for the update system.
type Config struct {
	// Enabled controls whether the update system is active.
	Enabled bool `yaml:"enabled"`

	// AutoCheck enables automatic update checks on startup.
	AutoCheck bool `yaml:"auto_check"`

	// CheckInterval is the minimum interval between automatic checks.
	CheckInterval time.Duration `yaml:"check_interval"`

	// AutoDownload enables automatic downloading of updates (but not installation).
	AutoDownload bool `yaml:"auto_download"`

	// IncludePrerelease includes beta/rc versions in update checks.
	IncludePrerelease bool `yaml:"include_prerelease"`

	// Channel is the update channel: stable, beta, nightly.
	Channel Channel `yaml:"channel"`

	// GitHubRepo is the GitHub repository in "owner/repo" format.
	GitHubRepo string `yaml:"github_repo"`

	// MaxBackups is the maximum number of backup versions to keep.
	MaxBackups int `yaml:"max_backups"`

	// VerifyChecksum enables checksum verification of downloaded binaries.
	VerifyChecksum bool `yaml:"verify_checksum"`

	// VerifySignature enables GPG signature verification (requires public key).
	VerifySignature bool `yaml:"verify_signature"`

	// PublicKeyPath is the path to the GPG public key for signature verification.
	PublicKeyPath string `yaml:"public_key_path,omitempty"`

	// Proxy is the HTTP proxy to use for update requests.
	Proxy string `yaml:"proxy,omitempty"`

	// Timeout is the timeout for HTTP requests.
	Timeout time.Duration `yaml:"timeout"`

	// NotifyOnly shows notification but doesn't prompt for update.
	NotifyOnly bool `yaml:"notify_only"`
}

// DefaultConfig returns the default update configuration.
func DefaultConfig() *Config {
	return &Config{
		Enabled:           true,
		AutoCheck:         true,
		CheckInterval:     24 * time.Hour,
		AutoDownload:      false,
		IncludePrerelease: false,
		Channel:           ChannelStable,
		GitHubRepo:        "user/gokin", // Should be updated to actual repo
		MaxBackups:        3,
		VerifyChecksum:    true,
		VerifySignature:   false,
		Timeout:           30 * time.Second,
		NotifyOnly:        false,
	}
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.GitHubRepo == "" {
		return ErrInvalidConfig
	}
	if c.CheckInterval < time.Minute {
		c.CheckInterval = time.Minute // Minimum 1 minute
	}
	if c.MaxBackups < 1 {
		c.MaxBackups = 1
	}
	if c.Timeout < 5*time.Second {
		c.Timeout = 5 * time.Second
	}
	return nil
}

// Merge merges another config into this one, preferring non-zero values.
func (c *Config) Merge(other *Config) {
	if other == nil {
		return
	}
	// Only merge non-default values
	if other.GitHubRepo != "" {
		c.GitHubRepo = other.GitHubRepo
	}
	if other.CheckInterval != 0 {
		c.CheckInterval = other.CheckInterval
	}
	if other.MaxBackups != 0 {
		c.MaxBackups = other.MaxBackups
	}
	if other.Timeout != 0 {
		c.Timeout = other.Timeout
	}
	if other.Proxy != "" {
		c.Proxy = other.Proxy
	}
	if other.PublicKeyPath != "" {
		c.PublicKeyPath = other.PublicKeyPath
	}
	if other.Channel != "" {
		c.Channel = other.Channel
	}
}

// ShouldCheck returns true if an update check should be performed.
func (c *Config) ShouldCheck(lastCheck time.Time) bool {
	if !c.Enabled || !c.AutoCheck {
		return false
	}
	return time.Since(lastCheck) >= c.CheckInterval
}

// MatchesChannel returns true if the release matches the configured channel.
func (c *Config) MatchesChannel(release *ReleaseInfo) bool {
	if release == nil {
		return false
	}

	switch c.Channel {
	case ChannelStable:
		return !release.Prerelease && !release.Draft
	case ChannelBeta:
		return !release.Draft // Include prereleases
	case ChannelNightly:
		return true // Include everything
	default:
		return !release.Prerelease && !release.Draft
	}
}
