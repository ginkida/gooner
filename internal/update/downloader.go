package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Downloader handles downloading update files.
type Downloader struct {
	httpClient *http.Client
	config     *Config
	tempDir    string
}

// NewDownloader creates a new downloader.
func NewDownloader(config *Config, tempDir string) *Downloader {
	return &Downloader{
		httpClient: &http.Client{
			Timeout: 0, // No timeout for downloads (can be large)
		},
		config:  config,
		tempDir: tempDir,
	}
}

// Download downloads a file from the given URL with progress reporting.
// Returns the path to the downloaded file.
func (d *Downloader) Download(ctx context.Context, url string, progress ProgressCallback) (string, error) {
	// Create request
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", "gokin-updater/1.0")
	req.Header.Set("Accept", "application/octet-stream")

	// Add GitHub token if available
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	// Send request
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: HTTP %d", ErrDownloadFailed, resp.StatusCode)
	}

	// Create temp directory if needed
	if err := os.MkdirAll(d.tempDir, 0755); err != nil {
		return "", err
	}

	// Create temp file
	tmpFile, err := os.CreateTemp(d.tempDir, "gokin-update-*")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	// Get total size for progress
	totalSize := resp.ContentLength

	// Create progress writer
	pw := &progressWriter{
		writer:   tmpFile,
		total:    totalSize,
		callback: progress,
	}

	// Download with progress
	_, err = io.Copy(pw, resp.Body)
	if err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("%w: %v", ErrDownloadFailed, err)
	}

	return tmpFile.Name(), nil
}

// DownloadChecksum downloads and parses a checksum file.
func (d *Downloader) DownloadChecksum(ctx context.Context, url string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", "gokin-updater/1.0")

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download checksum: HTTP %d", resp.StatusCode)
	}

	// Limit size of checksum file
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024)) // 1MB max
	if err != nil {
		return nil, err
	}

	return d.parseChecksumFile(string(data)), nil
}

// parseChecksumFile parses a checksum file in common formats.
// Supports: "checksum  filename" and "checksum filename" formats.
func (d *Downloader) parseChecksumFile(content string) map[string]string {
	checksums := make(map[string]string)

	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Try "checksum  filename" format (sha256sum output)
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			checksum := parts[0]
			filename := parts[len(parts)-1]
			// Remove leading * from binary mode indicator
			filename = strings.TrimPrefix(filename, "*")
			checksums[filename] = strings.ToLower(checksum)
		}
	}

	return checksums
}

// VerifyChecksum verifies the checksum of a file.
func (d *Downloader) VerifyChecksum(filePath, expectedChecksum string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}

	actualChecksum := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return fmt.Errorf("%w: expected %s, got %s", ErrChecksumMismatch, expectedChecksum, actualChecksum)
	}

	return nil
}

// ComputeChecksum computes the SHA256 checksum of a file.
func (d *Downloader) ComputeChecksum(filePath string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// ExtractBinary extracts the binary from an archive.
// Supports: .tar.gz, .tgz, .zip, and raw binaries.
func (d *Downloader) ExtractBinary(archivePath, binaryName string) (string, error) {
	ext := strings.ToLower(filepath.Ext(archivePath))

	// Check for .tar.gz
	if strings.HasSuffix(strings.ToLower(archivePath), ".tar.gz") || ext == ".tgz" {
		return d.extractTarGz(archivePath, binaryName)
	}

	if ext == ".zip" {
		return d.extractZip(archivePath, binaryName)
	}

	// Assume it's a raw binary
	return archivePath, nil
}

// extractTarGz extracts a binary from a tar.gz archive.
func (d *Downloader) extractTarGz(archivePath, binaryName string) (string, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		// Look for the binary
		baseName := filepath.Base(header.Name)
		if baseName == binaryName || baseName == binaryName+".exe" {
			if header.Typeflag == tar.TypeReg {
				// Extract to temp file
				outPath := filepath.Join(d.tempDir, baseName)
				outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, os.FileMode(header.Mode))
				if err != nil {
					return "", err
				}

				if _, err := io.Copy(outFile, tr); err != nil {
					outFile.Close()
					return "", err
				}
				outFile.Close()

				return outPath, nil
			}
		}
	}

	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

// extractZip extracts a binary from a zip archive.
func (d *Downloader) extractZip(archivePath, binaryName string) (string, error) {
	r, err := zip.OpenReader(archivePath)
	if err != nil {
		return "", err
	}
	defer r.Close()

	for _, f := range r.File {
		baseName := filepath.Base(f.Name)
		if baseName == binaryName || baseName == binaryName+".exe" {
			if !f.FileInfo().IsDir() {
				rc, err := f.Open()
				if err != nil {
					return "", err
				}

				outPath := filepath.Join(d.tempDir, baseName)
				outFile, err := os.OpenFile(outPath, os.O_CREATE|os.O_WRONLY, f.Mode())
				if err != nil {
					rc.Close()
					return "", err
				}

				if _, err := io.Copy(outFile, rc); err != nil {
					outFile.Close()
					rc.Close()
					return "", err
				}

				outFile.Close()
				rc.Close()

				return outPath, nil
			}
		}
	}

	return "", fmt.Errorf("binary %q not found in archive", binaryName)
}

// Cleanup removes temporary files.
func (d *Downloader) Cleanup() error {
	if d.tempDir == "" {
		return nil
	}
	return os.RemoveAll(d.tempDir)
}

// progressWriter wraps an io.Writer to report progress.
type progressWriter struct {
	writer   io.Writer
	total    int64
	written  int64
	callback ProgressCallback
}

func (pw *progressWriter) Write(p []byte) (int, error) {
	n, err := pw.writer.Write(p)
	pw.written += int64(n)

	if pw.callback != nil {
		var percent float64
		if pw.total > 0 {
			percent = float64(pw.written) / float64(pw.total) * 100
		}

		pw.callback(&UpdateProgress{
			Status:          StatusDownloading,
			BytesDownloaded: pw.written,
			TotalBytes:      pw.total,
			Percent:         percent,
		})
	}

	return n, err
}
