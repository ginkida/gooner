package security

import (
	"regexp"
	"strings"
)

// SecretRedactor masks sensitive information in strings using common patterns.
type SecretRedactor struct {
	patterns []*regexp.Regexp
	whitelist map[string]bool
}

// NewSecretRedactor creates a new redactor with default patterns for common secrets.
func NewSecretRedactor() *SecretRedactor {
	whitelist := map[string]bool{
		"true": true, "false": true, "null": true,
		"example": true, "test": true, "xxx": true,
		"localhost": true, "127.0.0.1": true,
		"0.0.0.0": true, "::1": true,
		"development": true, "staging": true, "production": true,
		"readme": true, "license": true,
	}

	return &SecretRedactor{
		whitelist: whitelist,
		patterns: []*regexp.Regexp{
			// ============================================================
			// API Keys and Tokens (with context)
			// ============================================================
			regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|auth[_-]?token|secret|password|passwd|pwd)[:=]\s*["']?([a-zA-Z0-9_\-\.]{8,})["']?`),

			// ============================================================
			// Bearer Tokens (limited to 256 chars, stops at delimiter)
			// ============================================================
			regexp.MustCompile(`(?i)Bearer\s+([a-zA-Z0-9_\-\.]{10,256})(?:\s|\"|\'|\r|\n|$)`),

			// ============================================================
			// AWS Keys
			// ============================================================
			regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
			regexp.MustCompile(`(?i)aws[_-]?secret[_-]?key[:=]\s*[a-zA-Z0-9+/]{40}`),

			// ============================================================
			// GitHub Tokens
			// ============================================================
			regexp.MustCompile(`gh[pous]_[a-zA-Z0-9]{36}`),

			// ============================================================
			// Stripe Keys
			// ============================================================
			regexp.MustCompile(`sk_live_[0-9a-zA-Z]{24}`),
			regexp.MustCompile(`sk_test_[0-9a-zA-Z]{24}`),

			// ============================================================
			// Google Cloud API Keys
			// ============================================================
			regexp.MustCompile(`AIza[0-9A-Za-z\-_]{35}`),

			// ============================================================
			// JWT Tokens (header.payload.signature)
			// ============================================================
			// JWT format: eyJ[base64].eyJ[base64].[signature]
			regexp.MustCompile(`eyJ[a-zA-Z0-9_-]+\.(?:eyJ[a-zA-Z0-9_-]+)?\.[a-zA-Z0-9_-]{20,}`),

			// ============================================================
			// PEM-encoded private keys (RSA, EC, ECDSA, Ed25519, etc.)
			// ============================================================
			// Covers: PRIVATE KEY, RSA PRIVATE KEY, EC PRIVATE KEY, etc.
			regexp.MustCompile(`-----BEGIN [A-Z]+ PRIVATE KEY-----[\s\S]+?-----END [A-Z]+ PRIVATE KEY-----`),
			regexp.MustCompile(`-----BEGIN [A-Z]+ KEY-----[\s\S]+?-----END [A-Z]+ KEY-----`),

			// ============================================================
			// Base64-encoded secrets (suspicious patterns)
			// ============================================================
			// Base64 strings 16+ chars that look like secrets
			// Must be preceded by key label or assignment
			regexp.MustCompile(`(?i)(?:api[_-]?key|secret|token|auth)[:=]\s*["']?[A-Za-z0-9+/]{16,}={0,2}["']?`),

			// ============================================================
			// Database URLs with passwords
			// ============================================================
			// postgres://user:password@host:port/db
			// mysql://user:password@host:port/db
			// No capture groups - full replacement
			regexp.MustCompile(`postgres://[^@]+:[^@]+@`),
			regexp.MustCompile(`mysql://[^@]+:[^@]+@`),
			regexp.MustCompile(`mongodb://[^@]+:[^@]+@`),

			// ============================================================
			// Redis URL with empty username (password-only)
			// ============================================================
			regexp.MustCompile(`redis://:[^@]+@`),

			// ============================================================
			// Connection strings with credentials
			// ============================================================
			// Server=host;Database=db;User=user;Password=secret;
			regexp.MustCompile(`(?i)(password|pwd)[:=]\s*[^;\'\"\s]{8,}`),

			// ============================================================
			// Slack Webhooks
			// ============================================================
			// hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXX
			regexp.MustCompile(`https?://hooks\.slack\.com/services/[A-Z0-9/]{30,}`),

			// ============================================================
			// Slack Bot Tokens
			// ============================================================
			// Format: xoxb-NUMBERS-NUMBERS-ALPHANUMERIC
			regexp.MustCompile(`xox[baprs]-[0-9]{10,}-[0-9]{10,}-[a-zA-Z0-9]{24}`),

			// ============================================================
			// Discord Bot Tokens
			// ============================================================
			// Format: BASE64.TIMESTAMP.HMAC
			regexp.MustCompile(`[MN][A-Za-z\d]{23}\.[\w-]{6}\.[\w-]{27}`),

			// ============================================================
			// Twilio Account Tokens
			// ============================================================
			// ACxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
			regexp.MustCompile(`AC[a-zA-Z0-9]{32}`),

			// ============================================================
			// Firebase Service Account (matches base64-like strings)
			// ============================================================
			// Usually very long base64 in JSON
			regexp.MustCompile(`(?i)"private[_-]?key"\s*:\s*"[^"]{100,}"`),

			// ============================================================
			// Private SSH keys (minimal pattern)
			// ============================================================
			regexp.MustCompile(`-----BEGIN [DR]SA PRIVATE KEY-----`),

			// ============================================================
			// Authorization headers with basic auth
			// ============================================================
			// Authorization: Basic base64(user:pass)
			regexp.MustCompile(`(?i)Authorization:\s*Basic\s+[A-Za-z0-9+/]{20,}={0,2}`),
		},
	}
}

// Redact masks all detected secrets in the input string.
func (r *SecretRedactor) Redact(text string) string {
	if text == "" {
		return ""
	}

	result := text
	for _, pattern := range r.patterns {
		if pattern.MatchString(result) {
			// If the pattern has submatches (like key=secret), we want to preserve the key part
			// and redact only the secret part.
			if pattern.NumSubexp() >= 1 {
				// We need a more complex replacement to preserve non-secret parts
				result = r.redactSubmatches(result, pattern)
			} else {
				result = pattern.ReplaceAllString(result, "[REDACTED]")
			}
		}
	}
	return result
}

// redactSubmatches handles patterns with capture groups (e.g., key=secret)
// Preserves the key name but redacts the secret value.
func (r *SecretRedactor) redactSubmatches(text string, pattern *regexp.Regexp) string {
	numGroups := pattern.NumSubexp()

	return pattern.ReplaceAllStringFunc(text, func(match string) string {
		subs := pattern.FindStringSubmatch(match)
		if len(subs) < 2 {
			return "[REDACTED]"
		}

		// subs[0] = full match (e.g., "apiKey: secret123")
		// subs[1] = first capture group (usually key name like "apiKey")
		// subs[2] = second capture group (usually the secret value)

		// Determine which capture group contains the secret
		// For patterns with 2+ groups, the last non-empty group is typically the secret
		var secret string
		var secretGroupIndex int

		// Find the last non-empty capture group (most likely the secret value)
		for i := numGroups; i >= 1; i-- {
			if i < len(subs) && subs[i] != "" {
				secret = subs[i]
				secretGroupIndex = i
				break
			}
		}

		if secret == "" {
			return "[REDACTED]"
		}

		// Check minimum length for secrets
		if len(secret) < 8 {
			return match // Don't redact short strings
		}

		// Check whitelist for safe values
		if r.isWhitelisted(secret) {
			return match // Don't redact safe values
		}

		// Replace only the secret part, preserving context
		// Find the position of the secret in the original match
		// We need to find where the capture group starts in the match
		result := match

		// For patterns with 2 groups (key=value), the second group is the secret
		// For patterns with 1 group, that group is the secret
		if numGroups >= 2 && secretGroupIndex == numGroups {
			// The secret is in the last group - replace it while keeping the prefix
			// Find where the secret appears in the match
			idx := strings.LastIndex(result, secret)
			if idx >= 0 {
				prefix := result[:idx]
				suffix := result[idx+len(secret):]
				result = prefix + "[REDACTED]" + suffix
			}
		} else if numGroups == 1 {
			// Single capture group - check if it looks like a key or a value
			// by looking for assignment operators before it
			idx := strings.Index(result, secret)
			if idx >= 0 {
				prefix := result[:idx]
				// If there's an assignment operator before the secret, it's a value
				if strings.ContainsAny(prefix, ":=") {
					suffix := result[idx+len(secret):]
					result = prefix + "[REDACTED]" + suffix
				} else {
					// No assignment operator - this might be a standalone secret
					// Replace the entire match
					result = "[REDACTED]"
				}
			}
		} else {
			// Multiple groups but secret is not in the last one
			// Replace the secret wherever it appears
			idx := strings.Index(result, secret)
			if idx >= 0 {
				prefix := result[:idx]
				suffix := result[idx+len(secret):]
				result = prefix + "[REDACTED]" + suffix
			}
		}

		return result
	})
}

// isWhitelisted checks if a value should not be redacted.
func (r *SecretRedactor) isWhitelisted(value string) bool {
	lower := strings.ToLower(value)
	lower = strings.Trim(lower, "\"'")
	
	if r.whitelist[lower] {
		return true
	}
	
	// Don't redact short words
	if len(lower) <= 4 {
		return true
	}

	// Don't redact obviously safe patterns
	safePatterns := []string{
		"example", "test", "demo", "sample", "mock",
		"localhost", "127.0.0.1", "::1",
		"dev", "staging", "prod",
		"readme", "license", "changelog",
		"config", "settings", "options",
		"database", "server", "host",
	}
	
	for _, safe := range safePatterns {
		if strings.Contains(lower, safe) {
			return true
		}
	}
	
	return false
}

// AddPattern adds a custom regex pattern to the redactor.
func (r *SecretRedactor) AddPattern(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}
	r.patterns = append(r.patterns, re)
	return nil
}

// RedactMap redacts values in a map that might contain secrets.
func (r *SecretRedactor) RedactMap(m map[string]any) map[string]any {
	redacted := make(map[string]any)
	for k, v := range m {
		if s, ok := v.(string); ok {
			redacted[k] = r.Redact(s)
		} else if nm, ok := v.(map[string]any); ok {
			redacted[k] = r.RedactMap(nm)
		} else {
			redacted[k] = v
		}
	}
	return redacted
}
