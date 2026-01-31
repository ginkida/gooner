package security

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// TLSVersion represents a TLS protocol version
type TLSVersion string

const (
	// TLSVersionAuto lets the Go standard library decide (currently TLS 1.2+)
	TLSVersionAuto TLSVersion = "auto"
	// TLSVersion12 forces TLS 1.2
	TLSVersion12 TLSVersion = "1.2"
	// TLSVersion13 forces TLS 1.3
	TLSVersion13 TLSVersion = "1.3"
)

// TLSConfig holds TLS configuration for HTTP clients
type TLSConfig struct {
	// MinVersion is the minimum TLS version to use
	MinVersion TLSVersion
	// MaxVersion is the maximum TLS version to use
	MaxVersion TLSVersion
	// InsecureSkipVerify controls certificate verification (default: false)
	// WARNING: Only set to true for testing with self-signed certs
	InsecureSkipVerify bool
	// HandshakeTimeout is the timeout for TLS handshake
	HandshakeTimeout time.Duration
}

// DefaultTLSConfig returns the default secure TLS configuration
func DefaultTLSConfig() TLSConfig {
	return TLSConfig{
		MinVersion:         TLSVersion12, // Require TLS 1.2 minimum
		MaxVersion:         TLSVersion13, // Allow up to TLS 1.3
		InsecureSkipVerify: false,        // Always verify certificates
		HandshakeTimeout:   10 * time.Second,
	}
}

// IsProductionMode checks if the application is running in production mode.
// It checks common environment variables used to indicate production.
func IsProductionMode() bool {
	env := strings.ToLower(os.Getenv("GO_ENV"))
	if env == "production" || env == "prod" {
		return true
	}

	env = strings.ToLower(os.Getenv("APP_ENV"))
	if env == "production" || env == "prod" {
		return true
	}

	env = strings.ToLower(os.Getenv("NODE_ENV"))
	if env == "production" {
		return true
	}

	env = strings.ToLower(os.Getenv("ENVIRONMENT"))
	if env == "production" || env == "prod" {
		return true
	}

	// Check for explicit production flag
	if os.Getenv("PRODUCTION") == "1" || strings.ToLower(os.Getenv("PRODUCTION")) == "true" {
		return true
	}

	return false
}

// CreateTLSConfig creates a crypto/tls.Config from TLSConfig
func CreateTLSConfig(cfg TLSConfig) (*tls.Config, error) {
	// Block InsecureSkipVerify in production mode
	if cfg.InsecureSkipVerify {
		if IsProductionMode() {
			return nil, fmt.Errorf("InsecureSkipVerify is not allowed in production mode. " +
				"Set GO_ENV, APP_ENV, or ENVIRONMENT to a non-production value to allow insecure TLS")
		}
		// Warn about insecure configuration in non-production
		fmt.Fprintf(os.Stderr, "WARNING: InsecureSkipVerify is enabled - TLS certificates will not be verified!\n")
		fmt.Fprintf(os.Stderr, "WARNING: This is only acceptable for development/testing with self-signed certificates.\n")
	}

	tlsCfg := &tls.Config{
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		MinVersion:         parseTLSVersion(cfg.MinVersion),
		MaxVersion:         parseTLSVersion(cfg.MaxVersion),
	}

	// Validate TLS version configuration
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		return nil, fmt.Errorf("minimum TLS version must be at least 1.2")
	}

	if tlsCfg.MaxVersion > tls.VersionTLS13 {
		return nil, fmt.Errorf("maximum TLS version must be at most 1.3")
	}

	if tlsCfg.MinVersion > tlsCfg.MaxVersion {
		return nil, fmt.Errorf("minimum TLS version cannot be greater than maximum")
	}

	// Use sensible cipher suites
	tlsCfg.CipherSuites = []uint16{
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_AES_128_GCM_SHA256,    // TLS 1.3
		tls.TLS_AES_256_GCM_SHA384,    // TLS 1.3
		tls.TLS_CHACHA20_POLY1305_SHA256, // TLS 1.3
	}

	// Prefer server cipher suites
	tlsCfg.PreferServerCipherSuites = true

	return tlsCfg, nil
}

// parseTLSVersion converts a TLSVersion string to uint16
func parseTLSVersion(version TLSVersion) uint16 {
	switch version {
	case TLSVersion12:
		return tls.VersionTLS12
	case TLSVersion13:
		return tls.VersionTLS13
	case TLSVersionAuto:
		// Return 0 to let Go's default behavior determine the version
		// Go 1.13+ defaults to TLS 1.2+
		return 0
	default:
		// Default to TLS 1.2 for security
		return tls.VersionTLS12
	}
}

// CreateSecureHTTPClient creates an HTTP client with secure TLS configuration
func CreateSecureHTTPClient(cfg TLSConfig, timeout time.Duration) (*http.Client, error) {
	// Create TLS configuration
	tlsCfg, err := CreateTLSConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to create TLS config: %w", err)
	}

	// Create HTTP transport with TLS configuration
	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		// Set reasonable defaults
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		// Disable HTTP/2 if needed (can be re-enabled)
		// ForceAttemptHTTP2: false,
	}

	// Configure dialer with timeout
	transport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext

	// Create HTTP client
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	return client, nil
}

// CreateDefaultHTTPClient creates an HTTP client with default secure settings
func CreateDefaultHTTPClient() (*http.Client, error) {
	cfg := DefaultTLSConfig()
	return CreateSecureHTTPClient(cfg, 30*time.Second)
}

// ValidateTLSConfig checks if a TLS configuration is secure
func ValidateTLSConfig(cfg *tls.Config) []string {
	warnings := []string{}

	// Check minimum version
	if cfg.MinVersion < tls.VersionTLS12 {
		warnings = append(warnings, fmt.Sprintf("minimum TLS version is 1.%d, should be at least 1.2", cfg.MinVersion>>8))
	}

	// Check if InsecureSkipVerify is enabled
	if cfg.InsecureSkipVerify {
		warnings = append(warnings, "InsecureSkipVerify is enabled - certificates will not be verified")
	}

	// Check for insecure cipher suites (basic check)
	if len(cfg.CipherSuites) == 0 {
		warnings = append(warnings, "no cipher suites specified - will use system defaults")
	}

	return warnings
}

// TLSVersionString returns a string representation of a TLS version number
func TLSVersionString(version uint16) string {
	switch version {
	case tls.VersionTLS10:
		return "TLS 1.0"
	case tls.VersionTLS11:
		return "TLS 1.1"
	case tls.VersionTLS12:
		return "TLS 1.2"
	case tls.VersionTLS13:
		return "TLS 1.3"
	default:
		return fmt.Sprintf("Unknown (0x%04x)", version)
	}
}
