package update

import (
	"time"
)

// ReleaseInfo contains information about a GitHub release.
type ReleaseInfo struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"` // Release notes (markdown)
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
	HTMLURL     string    `json:"html_url"`
}

// Version returns the version string (without 'v' prefix).
func (r *ReleaseInfo) Version() string {
	if len(r.TagName) > 0 && r.TagName[0] == 'v' {
		return r.TagName[1:]
	}
	return r.TagName
}

// Asset represents a downloadable file in a release.
type Asset struct {
	ID                 int64     `json:"id"`
	Name               string    `json:"name"`
	Label              string    `json:"label"`
	ContentType        string    `json:"content_type"`
	State              string    `json:"state"`
	Size               int64     `json:"size"`
	DownloadCount      int       `json:"download_count"`
	BrowserDownloadURL string    `json:"browser_download_url"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// DownloadURL returns the URL to download this asset.
func (a *Asset) DownloadURL() string {
	return a.BrowserDownloadURL
}

// UpdateInfo contains information about an available update.
type UpdateInfo struct {
	CurrentVersion string
	NewVersion     string
	ReleaseNotes   string
	ReleaseURL     string
	PublishedAt    time.Time
	AssetURL       string
	AssetSize      int64
	AssetName      string
	ChecksumURL    string // URL to checksum file (if available)
	IsPrerelease   bool
}

// UpdateStatus represents the current state of an update operation.
type UpdateStatus int

const (
	StatusIdle UpdateStatus = iota
	StatusChecking
	StatusDownloading
	StatusVerifying
	StatusInstalling
	StatusComplete
	StatusFailed
	StatusRolledBack
)

// String returns the string representation of UpdateStatus.
func (s UpdateStatus) String() string {
	switch s {
	case StatusIdle:
		return "idle"
	case StatusChecking:
		return "checking"
	case StatusDownloading:
		return "downloading"
	case StatusVerifying:
		return "verifying"
	case StatusInstalling:
		return "installing"
	case StatusComplete:
		return "complete"
	case StatusFailed:
		return "failed"
	case StatusRolledBack:
		return "rolled_back"
	default:
		return "unknown"
	}
}

// UpdateProgress represents progress of an update operation.
type UpdateProgress struct {
	Status          UpdateStatus
	Message         string
	BytesDownloaded int64
	TotalBytes      int64
	Percent         float64
	Error           error
}

// UpdateResult contains the result of an update operation.
type UpdateResult struct {
	Success         bool
	PreviousVer     string
	NewVer          string
	BackupPath      string
	Error           error
	RestartRequired bool
}

// BackupInfo contains information about a backup.
type BackupInfo struct {
	ID          string    `json:"id"`
	Version     string    `json:"version"`
	Path        string    `json:"path"`
	BinaryPath  string    `json:"binary_path"`
	CreatedAt   time.Time `json:"created_at"`
	InstalledAt time.Time `json:"installed_at,omitempty"`
	Size        int64     `json:"size"`
	Checksum    string    `json:"checksum"`
	Description string    `json:"description"`
}

// UpdateCache stores cached update check results.
type UpdateCache struct {
	LastCheck       time.Time    `json:"last_check"`
	LatestVersion   string       `json:"latest_version"`
	UpdateAvailable bool         `json:"update_available"`
	ReleaseNotes    string       `json:"release_notes,omitempty"`
	ReleaseURL      string       `json:"release_url,omitempty"`
	AssetURL        string       `json:"asset_url,omitempty"`
	AssetName       string       `json:"asset_name,omitempty"`
	PublishedAt     time.Time    `json:"published_at,omitempty"`
	ReleaseInfo     *ReleaseInfo `json:"release_info,omitempty"`
	Error           string       `json:"error,omitempty"`
}

// Channel represents the update channel.
type Channel string

const (
	ChannelStable  Channel = "stable"
	ChannelBeta    Channel = "beta"
	ChannelNightly Channel = "nightly"
)

// ProgressCallback is called during download to report progress.
type ProgressCallback func(progress *UpdateProgress)

// Platform represents a target platform for binaries.
type Platform struct {
	OS   string
	Arch string
}

// String returns the platform string (e.g., "linux_amd64").
func (p Platform) String() string {
	return p.OS + "_" + p.Arch
}

// AssetPattern returns the expected asset name pattern for this platform.
func (p Platform) AssetPattern() string {
	// Common patterns: gokin_linux_amd64, gokin-linux-amd64, gokin_linux_amd64.tar.gz
	return "gokin_" + p.OS + "_" + p.Arch
}

// AppInterface defines the interface for notifying the application of update availability.
type AppInterface interface {
	AddSystemMessage(msg string)
}
