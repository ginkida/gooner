package security

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// SSHValidator validates SSH commands and hosts for safety.
type SSHValidator struct {
	blockedCommands   []string
	blockedPatterns   []*regexp.Regexp
	blockedSubstrings []string
	allowedHosts      []string // If set, only these hosts are allowed
	blockedHosts      []string // Always blocked hosts
}

// NewSSHValidator creates a new SSH validator with secure defaults.
func NewSSHValidator() *SSHValidator {
	v := &SSHValidator{
		blockedCommands: []string{
			// Classic fork bombs
			":(){:|:&};:",
			":(){ :|:& };:",
		},
		blockedSubstrings: []string{
			// Destructive filesystem operations
			"rm -rf /",
			"rm -rf /*",
			"rm -fr /",
			"rm -fr /*",
			// Disk operations
			"mkfs.",
			"mkfs ",
			"> /dev/sda",
			"> /dev/nvme",
			"dd if=/dev/zero of=/dev/sd",
			"dd if=/dev/zero of=/dev/nvme",
			"dd if=/dev/urandom of=/dev/sd",
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
		blockedHosts: []string{
			// Block commonly sensitive hosts
			"localhost",
			"127.0.0.1",
			"::1",
		},
	}

	// Compile regex patterns
	v.blockedPatterns = []*regexp.Regexp{
		// Fork bomb patterns
		regexp.MustCompile(`:\s*\(\s*\)\s*\{`),
		regexp.MustCompile(`\$\{?0\}?\s*[&|]\s*\$\{?0\}?`),
		regexp.MustCompile(`while\s+true\s*;\s*do.*&`),
		regexp.MustCompile(`(?i)fork\s*bomb`),
		regexp.MustCompile(`\byes\s*\|\s*sh`),
		regexp.MustCompile(`\beval\s+\$\(`),
		regexp.MustCompile(`\bexec\s+\$\{?0\}?`),

		// Recursive deletion with variable expansion
		regexp.MustCompile(`rm\s+(-[rRf]+\s+)+/`),
		regexp.MustCompile(`rm\s+(-[rRf]+\s+)+\$`),

		// dd to block devices
		regexp.MustCompile(`dd\s+.*of=/dev/[snhv]d`),
		regexp.MustCompile(`dd\s+.*of=/dev/nvme`),

		// Wget/curl piped to shell (potential malware download)
		regexp.MustCompile(`(?i)(wget|curl)\s+.*\|\s*(ba)?sh`),

		// Base64 decode piped to shell
		regexp.MustCompile(`base64\s+-d.*\|\s*(ba)?sh`),

		// Python/Perl reverse shells
		regexp.MustCompile(`python[23]?\s+-c\s+['"].*socket.*exec`),
		regexp.MustCompile(`perl\s+-e\s+['"].*socket.*exec`),

		// crontab manipulation for persistence
		regexp.MustCompile(`echo\s+.*>>\s*/etc/cron`),

		// SSH key injection on remote host
		regexp.MustCompile(`echo\s+.*>>\s*.*authorized_keys`),

		// Systemd service creation (persistence)
		regexp.MustCompile(`cat\s+.*>\s*/etc/systemd/system/`),

		// History clearing (covering tracks)
		regexp.MustCompile(`>\s*~/\..*history`),
		regexp.MustCompile(`history\s+-c`),
		regexp.MustCompile(`unset\s+HISTFILE`),
	}

	return v
}

// SSHValidationResult contains the result of SSH validation.
type SSHValidationResult struct {
	Valid   bool
	Reason  string
	Pattern string
}

// ValidateCommand checks if a command is safe to execute via SSH.
func (v *SSHValidator) ValidateCommand(command string) SSHValidationResult {
	if command == "" {
		return SSHValidationResult{
			Valid:  false,
			Reason: "empty command",
		}
	}

	// Normalize the command for checking
	normalizedCmd := strings.ToLower(command)

	// Check exact blocked commands
	for _, blocked := range v.blockedCommands {
		if command == blocked || normalizedCmd == strings.ToLower(blocked) {
			return SSHValidationResult{
				Valid:   false,
				Reason:  "blocked command",
				Pattern: blocked,
			}
		}
	}

	// Check blocked substrings
	for _, substr := range v.blockedSubstrings {
		if strings.Contains(normalizedCmd, strings.ToLower(substr)) {
			return SSHValidationResult{
				Valid:   false,
				Reason:  fmt.Sprintf("contains blocked pattern: %s", substr),
				Pattern: substr,
			}
		}
	}

	// Check regex patterns
	for _, pattern := range v.blockedPatterns {
		if pattern.MatchString(command) {
			return SSHValidationResult{
				Valid:   false,
				Reason:  "matches dangerous pattern",
				Pattern: pattern.String(),
			}
		}
	}

	return SSHValidationResult{
		Valid:  true,
		Reason: "command passed validation",
	}
}

// ValidateHost checks if a host is allowed for SSH connections.
func (v *SSHValidator) ValidateHost(host string) SSHValidationResult {
	if host == "" {
		return SSHValidationResult{
			Valid:  false,
			Reason: "empty host",
		}
	}

	// Normalize host
	normalizedHost := strings.ToLower(strings.TrimSpace(host))

	// Check blocked hosts
	for _, blocked := range v.blockedHosts {
		if normalizedHost == strings.ToLower(blocked) {
			return SSHValidationResult{
				Valid:   false,
				Reason:  fmt.Sprintf("blocked host: %s", blocked),
				Pattern: blocked,
			}
		}
	}

	// Check if host resolves to a loopback address
	ips, err := net.LookupIP(normalizedHost)
	if err == nil {
		for _, ip := range ips {
			if ip.IsLoopback() {
				return SSHValidationResult{
					Valid:  false,
					Reason: "host resolves to loopback address",
				}
			}
		}
	}

	// If allowed hosts are set, check against whitelist
	if len(v.allowedHosts) > 0 {
		allowed := false
		for _, h := range v.allowedHosts {
			if normalizedHost == strings.ToLower(h) {
				allowed = true
				break
			}
		}
		if !allowed {
			return SSHValidationResult{
				Valid:  false,
				Reason: "host not in allowed list",
			}
		}
	}

	return SSHValidationResult{
		Valid:  true,
		Reason: "host passed validation",
	}
}

// SetAllowedHosts sets the whitelist of allowed hosts.
// If set, only these hosts can be connected to.
func (v *SSHValidator) SetAllowedHosts(hosts []string) {
	v.allowedHosts = hosts
}

// AddBlockedHost adds a host to the blocklist.
func (v *SSHValidator) AddBlockedHost(host string) {
	v.blockedHosts = append(v.blockedHosts, host)
}

// AddBlockedCommand adds a command to the blocklist.
func (v *SSHValidator) AddBlockedCommand(cmd string) {
	v.blockedCommands = append(v.blockedCommands, cmd)
}

// AddBlockedSubstring adds a substring pattern to the blocklist.
func (v *SSHValidator) AddBlockedSubstring(substr string) {
	v.blockedSubstrings = append(v.blockedSubstrings, substr)
}

// AddBlockedPattern adds a regex pattern to the blocklist.
func (v *SSHValidator) AddBlockedPattern(pattern string) error {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Errorf("invalid regex pattern: %w", err)
	}
	v.blockedPatterns = append(v.blockedPatterns, re)
	return nil
}

// DefaultSSHValidator is a singleton validator with default security rules.
var DefaultSSHValidator = NewSSHValidator()

// ValidateSSHCommand is a convenience function using the default validator.
func ValidateSSHCommand(command string) SSHValidationResult {
	return DefaultSSHValidator.ValidateCommand(command)
}

// ValidateSSHHost is a convenience function using the default validator.
func ValidateSSHHost(host string) SSHValidationResult {
	return DefaultSSHValidator.ValidateHost(host)
}
