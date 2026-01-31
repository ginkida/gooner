package security

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// IPValidator provides SSRF protection by validating URLs and IP addresses.
// It blocks requests to private networks, localhost, and link-local addresses.
type IPValidator struct {
	// blockedNetworks are CIDR ranges that should not be accessed
	blockedNetworks []*net.IPNet
	// allowedSchemes are URL schemes that are permitted
	allowedSchemes map[string]bool
	// blockedSchemes are URL schemes that are explicitly blocked
	blockedSchemes map[string]bool
}

// NewIPValidator creates a new IPValidator with secure defaults.
func NewIPValidator() *IPValidator {
	v := &IPValidator{
		allowedSchemes: map[string]bool{
			"http":  true,
			"https": true,
		},
		blockedSchemes: map[string]bool{
			"file":       true,
			"ftp":        true,
			"gopher":     true,
			"data":       true,
			"javascript": true,
			"vbscript":   true,
			"dict":       true,
			"ldap":       true,
			"ldaps":      true,
		},
	}

	// Define blocked CIDR ranges
	blockedCIDRs := []string{
		// IPv4 private ranges
		"10.0.0.0/8",       // Class A private
		"172.16.0.0/12",    // Class B private
		"192.168.0.0/16",   // Class C private
		"127.0.0.0/8",      // Loopback
		"169.254.0.0/16",   // Link-local
		"100.64.0.0/10",    // Carrier-grade NAT
		"192.0.0.0/24",     // IETF Protocol Assignments
		"192.0.2.0/24",     // TEST-NET-1
		"198.51.100.0/24",  // TEST-NET-2
		"203.0.113.0/24",   // TEST-NET-3
		"192.88.99.0/24",   // 6to4 Relay Anycast
		"198.18.0.0/15",    // Network benchmark tests
		"224.0.0.0/4",      // Multicast
		"240.0.0.0/4",      // Reserved for future use
		"255.255.255.255/32", // Broadcast

		// IPv6 private/special ranges
		"::1/128",          // Loopback
		"fe80::/10",        // Link-local
		"fc00::/7",         // Unique local address
		"ff00::/8",         // Multicast
		"::ffff:0:0/96",    // IPv4-mapped IPv6 (needs special handling)
		"64:ff9b::/96",     // NAT64
		"100::/64",         // Discard prefix
		"2001:db8::/32",    // Documentation
		"2001::/32",        // Teredo
		"2002::/16",        // 6to4
	}

	v.blockedNetworks = make([]*net.IPNet, 0, len(blockedCIDRs))
	for _, cidr := range blockedCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err == nil {
			v.blockedNetworks = append(v.blockedNetworks, network)
		}
	}

	return v
}

// URLValidationResult contains the result of URL validation.
type URLValidationResult struct {
	Valid      bool
	Reason     string
	ResolvedIP net.IP // The resolved IP address, if DNS resolution was performed
}

// ValidateURL validates a URL for SSRF protection.
// It checks the scheme, resolves the hostname, and validates the IP address.
func (v *IPValidator) ValidateURL(rawURL string) URLValidationResult {
	if rawURL == "" {
		return URLValidationResult{
			Valid:  false,
			Reason: "empty URL",
		}
	}

	// Parse the URL
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		return URLValidationResult{
			Valid:  false,
			Reason: fmt.Sprintf("invalid URL: %s", err),
		}
	}

	// Validate scheme
	scheme := strings.ToLower(parsedURL.Scheme)
	if v.blockedSchemes[scheme] {
		return URLValidationResult{
			Valid:  false,
			Reason: fmt.Sprintf("blocked URL scheme: %s", scheme),
		}
	}

	if !v.allowedSchemes[scheme] {
		return URLValidationResult{
			Valid:  false,
			Reason: fmt.Sprintf("unsupported URL scheme: %s", scheme),
		}
	}

	// Extract hostname
	hostname := parsedURL.Hostname()
	if hostname == "" {
		return URLValidationResult{
			Valid:  false,
			Reason: "missing hostname",
		}
	}

	// Check for obviously blocked hostnames
	lowerHost := strings.ToLower(hostname)
	if lowerHost == "localhost" || lowerHost == "localhost.localdomain" {
		return URLValidationResult{
			Valid:  false,
			Reason: "localhost is not allowed",
		}
	}

	// Check if it's a raw IP address
	if ip := net.ParseIP(hostname); ip != nil {
		if v.isBlockedIP(ip) {
			return URLValidationResult{
				Valid:      false,
				Reason:     fmt.Sprintf("blocked IP address: %s", ip),
				ResolvedIP: ip,
			}
		}
		return URLValidationResult{
			Valid:      true,
			Reason:     "IP address is allowed",
			ResolvedIP: ip,
		}
	}

	// Resolve hostname to IP addresses
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return URLValidationResult{
			Valid:  false,
			Reason: fmt.Sprintf("DNS resolution failed: %s", err),
		}
	}

	if len(ips) == 0 {
		return URLValidationResult{
			Valid:  false,
			Reason: "hostname resolved to no IP addresses",
		}
	}

	// Check ALL resolved IPs (defense against DNS rebinding)
	for _, ip := range ips {
		if v.isBlockedIP(ip) {
			return URLValidationResult{
				Valid:      false,
				Reason:     fmt.Sprintf("hostname resolves to blocked IP: %s", ip),
				ResolvedIP: ip,
			}
		}
	}

	// Use the first IP for the result
	return URLValidationResult{
		Valid:      true,
		Reason:     "URL is allowed",
		ResolvedIP: ips[0],
	}
}

// isBlockedIP checks if an IP address is in any blocked network.
func (v *IPValidator) isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true // Nil IP is blocked
	}

	// Check against all blocked networks
	for _, network := range v.blockedNetworks {
		if network.Contains(ip) {
			return true
		}
	}

	// Additional check for IPv4-mapped IPv6 addresses
	// These are in the form ::ffff:192.168.1.1
	if ip4 := ip.To4(); ip4 != nil {
		// Re-check as IPv4
		for _, network := range v.blockedNetworks {
			if network.Contains(ip4) {
				return true
			}
		}
	}

	return false
}

// ValidateIP validates an IP address directly.
func (v *IPValidator) ValidateIP(ip net.IP) URLValidationResult {
	if ip == nil {
		return URLValidationResult{
			Valid:  false,
			Reason: "nil IP address",
		}
	}

	if v.isBlockedIP(ip) {
		return URLValidationResult{
			Valid:      false,
			Reason:     fmt.Sprintf("blocked IP address: %s", ip),
			ResolvedIP: ip,
		}
	}

	return URLValidationResult{
		Valid:      true,
		Reason:     "IP address is allowed",
		ResolvedIP: ip,
	}
}

// ValidateHostname validates a hostname by resolving it and checking the IPs.
func (v *IPValidator) ValidateHostname(hostname string) URLValidationResult {
	if hostname == "" {
		return URLValidationResult{
			Valid:  false,
			Reason: "empty hostname",
		}
	}

	// Check for obviously blocked hostnames
	lowerHost := strings.ToLower(hostname)
	if lowerHost == "localhost" || lowerHost == "localhost.localdomain" {
		return URLValidationResult{
			Valid:  false,
			Reason: "localhost is not allowed",
		}
	}

	// Check if it's already an IP
	if ip := net.ParseIP(hostname); ip != nil {
		return v.ValidateIP(ip)
	}

	// Resolve hostname
	ips, err := net.LookupIP(hostname)
	if err != nil {
		return URLValidationResult{
			Valid:  false,
			Reason: fmt.Sprintf("DNS resolution failed: %s", err),
		}
	}

	if len(ips) == 0 {
		return URLValidationResult{
			Valid:  false,
			Reason: "hostname resolved to no IP addresses",
		}
	}

	// Check all resolved IPs
	for _, ip := range ips {
		if v.isBlockedIP(ip) {
			return URLValidationResult{
				Valid:      false,
				Reason:     fmt.Sprintf("hostname resolves to blocked IP: %s", ip),
				ResolvedIP: ip,
			}
		}
	}

	return URLValidationResult{
		Valid:      true,
		Reason:     "hostname is allowed",
		ResolvedIP: ips[0],
	}
}

// AddBlockedNetwork adds a CIDR range to the blocklist.
func (v *IPValidator) AddBlockedNetwork(cidr string) error {
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("invalid CIDR: %w", err)
	}
	v.blockedNetworks = append(v.blockedNetworks, network)
	return nil
}

// AllowScheme adds a URL scheme to the allowed list.
func (v *IPValidator) AllowScheme(scheme string) {
	v.allowedSchemes[strings.ToLower(scheme)] = true
	delete(v.blockedSchemes, strings.ToLower(scheme))
}

// BlockScheme adds a URL scheme to the blocked list.
func (v *IPValidator) BlockScheme(scheme string) {
	v.blockedSchemes[strings.ToLower(scheme)] = true
	delete(v.allowedSchemes, strings.ToLower(scheme))
}

// DefaultIPValidator is a singleton validator with default security rules.
var DefaultIPValidator = NewIPValidator()

// ValidateURLForSSRF is a convenience function using the default validator.
func ValidateURLForSSRF(rawURL string) URLValidationResult {
	return DefaultIPValidator.ValidateURL(rawURL)
}
