package security

import (
	"fmt"
	"regexp"
	"strings"
)

// CommandValidator provides unified command validation for bash execution.
// It implements both whitelist and blocklist approaches for maximum security.
type CommandValidator struct {
	// blockedPatterns are regex patterns that should never be allowed
	blockedPatterns []*regexp.Regexp
	// cautionPatterns are patterns that are suspicious but not outright blocked
	cautionPatterns []*regexp.Regexp
	// blockedCommands are exact command strings that are blocked
	blockedCommands []string
	// blockedSubstrings are substrings that indicate dangerous commands
	blockedSubstrings []string
}

// NewCommandValidator creates a new CommandValidator with secure defaults.
func NewCommandValidator() *CommandValidator {
	cv := &CommandValidator{
		blockedCommands: []string{
			// Classic fork bombs
			":(){:|:&};:",
			":(){ :|:& };:",
		},
		blockedSubstrings: []string{
			// Destructive filesystem operations
			"rm -rf /",
			"rm -rf /*",
			"rm -rf ~",
			"rm -rf $HOME",
			"rm -rf ${HOME}",
			"rm -fr /",
			"rm -fr /*",
			// Disk operations
			"mkfs.",
			"mkfs ",
			"> /dev/sda",
			"> /dev/nvme",
			"> /dev/hd",
			"> /dev/vd",
			"dd if=/dev/zero of=/dev/sd",
			"dd if=/dev/zero of=/dev/nvme",
			"dd if=/dev/zero of=/dev/hd",
			"dd if=/dev/zero of=/dev/vd",
			"dd if=/dev/urandom of=/dev/sd",
			"dd if=/dev/urandom of=/dev/nvme",
			"dd if=/dev/random of=/dev/sd",
			// Permission attacks
			"chmod -R 777 /",
			"chmod 777 /",
			"chown -R root /",
			// Network attacks / reverse shells
			"nc -e",
			"nc -c",
			"ncat -e",
			"ncat -c",
			"bash -i >& /dev/tcp",
			"bash -i >& /dev/udp",
			"/dev/tcp/",
			"/dev/udp/",
			// Sensitive file access
			"/etc/shadow",
			"/etc/passwd",
			".ssh/id_rsa",
			".ssh/id_ed25519",
			".ssh/id_ecdsa",
			".ssh/id_dsa",
			".aws/credentials",
			".kube/config",
			".gnupg/",
			// Kernel/system modification
			"insmod ",
			"rmmod ",
			"modprobe ",
			"/proc/sys",
			"/sys/kernel",
			// Boot modification
			"/boot/",
			"grub-install",
			"update-grub",
			// Credential theft
			"mimikatz",
			"hashdump",
			"secretsdump",
		},
	}

	// Compile regex patterns for more complex detection
	cv.blockedPatterns = []*regexp.Regexp{
		// Fork bomb patterns
		regexp.MustCompile(`:\s*\(\s*\)\s*\{`),                // :(){
		regexp.MustCompile(`\$\{?0\}?\s*[&|]\s*\$\{?0\}?`),    // $0 & $0 or $0 | $0
		regexp.MustCompile(`while\s+true\s*;\s*do.*&`),        // while true; do ... &
		regexp.MustCompile(`(?i)fork\s*bomb`),                 // "fork bomb" mention
		regexp.MustCompile(`\byes\s*\|\s*sh`),                 // yes | sh
		regexp.MustCompile(`\beval\s+\$\(`),                   // eval $(...)
		regexp.MustCompile(`\bexec\s+\$\{?0\}?`),              // exec $0

		// Recursive deletion with variable expansion
		regexp.MustCompile(`rm\s+(-[rRf]+\s+)+/`),             // rm -rf / variants
		regexp.MustCompile(`rm\s+(-[rRf]+\s+)+\$`),            // rm -rf $VAR

		// dd to block devices
		regexp.MustCompile(`dd\s+.*of=/dev/[snhv]d`),          // dd of=/dev/sd*
		regexp.MustCompile(`dd\s+.*of=/dev/nvme`),             // dd of=/dev/nvme*

		// Wget/curl piped to shell (potential malware download)
		regexp.MustCompile(`(?i)(wget|curl)\s+.*\|\s*(ba)?sh`),
		regexp.MustCompile(`(?i)(wget|curl)\s+-[^|]*\|\s*(ba)?sh`),

		// Base64 decode piped to shell
		regexp.MustCompile(`base64\s+-d.*\|\s*(ba)?sh`),

		// Python/Perl one-liners that could be reverse shells
		regexp.MustCompile(`python[23]?\s+-c\s+['"].*socket.*exec`),
		regexp.MustCompile(`perl\s+-e\s+['"].*socket.*exec`),

		// Overwriting MBR/bootloader
		regexp.MustCompile(`dd\s+.*of=/dev/[snhv]d[a-z]$`),    // dd to disk (no partition)

		// Mounting attacks
		regexp.MustCompile(`mount\s+.*-o\s+.*remount.*rw\s+/`),

		// crontab manipulation for persistence
		regexp.MustCompile(`echo\s+.*>>\s*/etc/cron`),
		regexp.MustCompile(`echo\s+.*>>\s*/var/spool/cron`),

		// SSH key injection
		regexp.MustCompile(`echo\s+.*>>\s*.*authorized_keys`),

		// Systemd service creation (persistence)
		regexp.MustCompile(`cat\s+.*>\s*/etc/systemd/system/`),

		// History clearing (covering tracks)
		regexp.MustCompile(`>\s*~/\..*history`),
		regexp.MustCompile(`history\s+-c`),
		regexp.MustCompile(`unset\s+HISTFILE`),

		// Process hiding attempts
		regexp.MustCompile(`LD_PRELOAD.*\.so`),

		// Obfuscated commands (hex/octal escapes)
		regexp.MustCompile(`\\[0-7]{3}`),                       // \OOO
		regexp.MustCompile(`(?i)printf\s+.*\\`),                // printf with escapes

		// Shell injection separators and constructs
		regexp.MustCompile(`[;&|]\s*(ba)?sh`),                 // ;sh, |sh, &sh
		regexp.MustCompile(`(?i)eval\s+.*(base64|curl|wget|nc\b)`), // eval with known-dangerous commands
		regexp.MustCompile(`>\s*/dev/(tcp|udp)/`),              // redundant but safer with separators
	}

	// Caution patterns: suspicious but not outright blocked.
	// These are checked only by ValidateWithLevel() for elevated permission prompts.
	cv.cautionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`\\x[0-9a-fA-F]{2}`),   // \xXX hex escapes
		regexp.MustCompile(`\$\(\s*.*\s*\)`),       // $( ... ) command substitution
		regexp.MustCompile("`[^`]*`"),               // `...` backtick substitution
		regexp.MustCompile(`\$\{[^}]+\}`),           // ${...} parameter expansion
	}

	return cv
}

// ValidationResult contains the result of command validation.
type ValidationResult struct {
	Valid   bool
	Reason  string
	Pattern string // The pattern that matched, if any
}

// Validate checks if a command is safe to execute.
// Returns a ValidationResult indicating whether the command is valid and why.
func (cv *CommandValidator) Validate(command string) ValidationResult {
	if command == "" {
		return ValidationResult{
			Valid:  false,
			Reason: "empty command",
		}
	}

	// Normalize the command for checking
	normalizedCmd := strings.ToLower(command)

	// Check exact blocked commands
	for _, blocked := range cv.blockedCommands {
		if command == blocked || normalizedCmd == strings.ToLower(blocked) {
			return ValidationResult{
				Valid:   false,
				Reason:  "blocked command",
				Pattern: blocked,
			}
		}
	}

	// Check blocked substrings
	for _, substr := range cv.blockedSubstrings {
		if strings.Contains(normalizedCmd, strings.ToLower(substr)) {
			return ValidationResult{
				Valid:   false,
				Reason:  fmt.Sprintf("contains blocked pattern: %s", substr),
				Pattern: substr,
			}
		}
	}

	// Check regex patterns
	for _, pattern := range cv.blockedPatterns {
		if pattern.MatchString(command) {
			return ValidationResult{
				Valid:   false,
				Reason:  "matches dangerous pattern",
				Pattern: pattern.String(),
			}
		}
	}

	return ValidationResult{
		Valid:  true,
		Reason: "command passed validation",
	}
}

// ValidateWithLevel checks a command and returns validation result with safety level.
// The level is one of: "blocked", "caution", or "safe".
func (cv *CommandValidator) ValidateWithLevel(command string) (ValidationResult, string) {
	result := cv.Validate(command)
	if !result.Valid {
		return result, "blocked"
	}

	// Check caution patterns
	normalized := strings.ToLower(command)
	for _, pattern := range cv.cautionPatterns {
		if pattern.MatchString(normalized) || pattern.MatchString(command) {
			return ValidationResult{
				Valid:   true,
				Reason:  "command contains potentially dangerous constructs",
				Pattern: pattern.String(),
			}, "caution"
		}
	}

	return result, "safe"
}

// AddBlockedCommand adds a command to the blocklist.
func (cv *CommandValidator) AddBlockedCommand(cmd string) {
	cv.blockedCommands = append(cv.blockedCommands, cmd)
}

// AddBlockedSubstring adds a substring pattern to the blocklist.
func (cv *CommandValidator) AddBlockedSubstring(substr string) {
	cv.blockedSubstrings = append(cv.blockedSubstrings, substr)
}

// AddBlockedPattern adds a regex pattern to the blocklist.
func (cv *CommandValidator) AddBlockedPattern(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid regex pattern: %w", err)
	}
	cv.blockedPatterns = append(cv.blockedPatterns, re)
	return nil
}

// DefaultCommandValidator is a singleton validator with default security rules.
var DefaultCommandValidator = NewCommandValidator()

// ValidateCommand is a convenience function using the default validator.
func ValidateCommand(command string) ValidationResult {
	return DefaultCommandValidator.Validate(command)
}
