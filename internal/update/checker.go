package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Checker handles version checking against GitHub releases.
type Checker struct {
	httpClient *http.Client
	repo       string
	cacheDir   string
	config     *Config
}

// NewChecker creates a new version checker.
func NewChecker(config *Config, cacheDir string) *Checker {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// Set proxy if configured
	if config.Proxy != "" {
		// Proxy configuration would be set here
	}

	client := &http.Client{
		Timeout:   config.Timeout,
		Transport: transport,
	}

	return &Checker{
		httpClient: client,
		repo:       config.GitHubRepo,
		cacheDir:   cacheDir,
		config:     config,
	}
}

// GetLatestRelease fetches the latest release from GitHub.
func (c *Checker) GetLatestRelease(ctx context.Context) (*ReleaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", c.repo)

	release, err := c.fetchRelease(ctx, url)
	if err != nil {
		return nil, err
	}

	// If prereleases not included and this is a prerelease, fetch all and find latest stable
	if !c.config.IncludePrerelease && release.Prerelease {
		return c.getLatestStableRelease(ctx)
	}

	return release, nil
}

// getLatestStableRelease finds the latest stable (non-prerelease) release.
func (c *Checker) getLatestStableRelease(ctx context.Context) (*ReleaseInfo, error) {
	releases, err := c.GetReleases(ctx, 20)
	if err != nil {
		return nil, err
	}

	for _, release := range releases {
		if !release.Prerelease && !release.Draft && c.config.MatchesChannel(&release) {
			r := release // Copy to avoid reference issues
			return &r, nil
		}
	}

	return nil, ErrNoReleases
}

// GetReleases fetches multiple releases from GitHub.
func (c *Checker) GetReleases(ctx context.Context, limit int) ([]ReleaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", c.repo, limit)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNetworkError, err)
	}
	defer resp.Body.Close()

	if err := c.checkResponse(resp); err != nil {
		return nil, err
	}

	var releases []ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("failed to parse releases: %w", err)
	}

	return releases, nil
}

// GetReleaseByTag fetches a specific release by tag.
func (c *Checker) GetReleaseByTag(ctx context.Context, tag string) (*ReleaseInfo, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", c.repo, tag)
	return c.fetchRelease(ctx, url)
}

// fetchRelease performs the HTTP request to fetch a release.
func (c *Checker) fetchRelease(ctx context.Context, url string) (*ReleaseInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	c.setHeaders(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNetworkError, err)
	}
	defer resp.Body.Close()

	if err := c.checkResponse(resp); err != nil {
		return nil, err
	}

	var release ReleaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("failed to parse release: %w", err)
	}

	return &release, nil
}

// setHeaders sets common headers for GitHub API requests.
func (c *Checker) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "gokin-updater/1.0")

	// Add GitHub token if available (increases rate limit)
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
}

// checkResponse checks the HTTP response for errors.
func (c *Checker) checkResponse(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrNoReleases
	case http.StatusForbidden:
		// Check if rate limited
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return ErrRateLimited
		}
		return ErrPermissionDenied
	case http.StatusUnauthorized:
		return ErrPermissionDenied
	default:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("GitHub API error: %s - %s", resp.Status, string(body))
	}
}

// FindAssetForPlatform finds the appropriate asset for the current platform.
func (c *Checker) FindAssetForPlatform(release *ReleaseInfo) *Asset {
	if release == nil || len(release.Assets) == 0 {
		return nil
	}

	platform := Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
	pattern := platform.AssetPattern()

	// Try exact match first
	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.Contains(strings.ToLower(asset.Name), strings.ToLower(pattern)) {
			return asset
		}
	}

	// Try alternative patterns
	alternatives := c.getAlternativePatterns(platform)
	for _, alt := range alternatives {
		for i := range release.Assets {
			asset := &release.Assets[i]
			if strings.Contains(strings.ToLower(asset.Name), strings.ToLower(alt)) {
				return asset
			}
		}
	}

	return nil
}

// getAlternativePatterns returns alternative asset name patterns.
func (c *Checker) getAlternativePatterns(platform Platform) []string {
	var patterns []string

	// Common alternative naming conventions
	// gokin-linux-amd64, gokin_linux_amd64, gokin-linux-x86_64
	patterns = append(patterns, fmt.Sprintf("gokin-%s-%s", platform.OS, platform.Arch))

	// Map amd64 to x86_64
	if platform.Arch == "amd64" {
		patterns = append(patterns, fmt.Sprintf("gokin_%s_x86_64", platform.OS))
		patterns = append(patterns, fmt.Sprintf("gokin-%s-x86_64", platform.OS))
	}

	// Map arm64 to aarch64
	if platform.Arch == "arm64" {
		patterns = append(patterns, fmt.Sprintf("gokin_%s_aarch64", platform.OS))
		patterns = append(patterns, fmt.Sprintf("gokin-%s-aarch64", platform.OS))
	}

	// Darwin -> macos/macOS
	if platform.OS == "darwin" {
		patterns = append(patterns, fmt.Sprintf("gokin_macos_%s", platform.Arch))
		patterns = append(patterns, fmt.Sprintf("gokin-macos-%s", platform.Arch))
		patterns = append(patterns, fmt.Sprintf("gokin_macOS_%s", platform.Arch))
	}

	return patterns
}

// FindChecksumAsset finds the checksum file for the given asset.
func (c *Checker) FindChecksumAsset(release *ReleaseInfo, asset *Asset) *Asset {
	if release == nil || asset == nil {
		return nil
	}

	// Common checksum file patterns
	checksumPatterns := []string{
		asset.Name + ".sha256",
		asset.Name + ".sha256sum",
		"checksums.txt",
		"SHA256SUMS",
		"sha256sums.txt",
	}

	for _, pattern := range checksumPatterns {
		for i := range release.Assets {
			a := &release.Assets[i]
			if strings.EqualFold(a.Name, pattern) {
				return a
			}
		}
	}

	return nil
}

// LoadCache loads cached update information.
func (c *Checker) LoadCache() (*UpdateCache, error) {
	cachePath := c.getCachePath()
	data, err := os.ReadFile(cachePath)
	if err != nil {
		return nil, err
	}

	var cache UpdateCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, err
	}

	return &cache, nil
}

// SaveCache saves update information to cache.
func (c *Checker) SaveCache(cache *UpdateCache) error {
	cachePath := c.getCachePath()

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(cachePath), 0755); err != nil {
		return err
	}

	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(cachePath, data, 0644)
}

// getCachePath returns the path to the cache file.
func (c *Checker) getCachePath() string {
	return filepath.Join(c.cacheDir, "update_cache.json")
}

// IsCacheValid returns true if cached data is still valid.
func (c *Checker) IsCacheValid(cache *UpdateCache) bool {
	if cache == nil {
		return false
	}
	return time.Since(cache.LastCheck) < c.config.CheckInterval
}
