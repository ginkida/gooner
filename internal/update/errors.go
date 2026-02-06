package update

import "errors"

// Common errors for the update package.
var (
	// ErrNoUpdate indicates no update is available.
	ErrNoUpdate = errors.New("no update available")

	// ErrUpdateDisabled indicates the update system is disabled.
	ErrUpdateDisabled = errors.New("update system is disabled")

	// ErrNoReleases indicates no releases were found.
	ErrNoReleases = errors.New("no releases found")

	// ErrNoAsset indicates no suitable asset was found for the platform.
	ErrNoAsset = errors.New("no suitable asset found for this platform")

	// ErrDownloadFailed indicates the download failed.
	ErrDownloadFailed = errors.New("download failed")

	// ErrChecksumMismatch indicates the checksum verification failed.
	ErrChecksumMismatch = errors.New("checksum mismatch")

	// ErrSignatureInvalid indicates the signature verification failed.
	ErrSignatureInvalid = errors.New("signature verification failed")

	// ErrInstallFailed indicates the installation failed.
	ErrInstallFailed = errors.New("installation failed")

	// ErrRollbackFailed indicates the rollback failed.
	ErrRollbackFailed = errors.New("rollback failed")

	// ErrNoBackup indicates no backup is available for rollback.
	ErrNoBackup = errors.New("no backup available")

	// ErrInvalidConfig indicates invalid configuration.
	ErrInvalidConfig = errors.New("invalid update configuration")

	// ErrPermissionDenied indicates insufficient permissions.
	ErrPermissionDenied = errors.New("permission denied")

	// ErrNetworkError indicates a network error.
	ErrNetworkError = errors.New("network error")

	// ErrRateLimited indicates rate limiting by GitHub API.
	ErrRateLimited = errors.New("rate limited by GitHub API")

	// ErrUpdateInProgress indicates an update is already in progress.
	ErrUpdateInProgress = errors.New("update already in progress")

	// ErrCancelled indicates the operation was cancelled.
	ErrCancelled = errors.New("operation cancelled")

	// ErrCorruptBinary indicates the downloaded binary is corrupt.
	ErrCorruptBinary = errors.New("downloaded binary is corrupt")

	// ErrVersionParse indicates a version parsing error.
	ErrVersionParse = errors.New("failed to parse version")

	// ErrSameVersion indicates the versions are the same.
	ErrSameVersion = errors.New("already running the latest version")
)
