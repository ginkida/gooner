package contract

import (
	"fmt"
	"strings"
	"time"
)

// ContractStatus represents the lifecycle state of a contract.
type ContractStatus int

const (
	StatusDraft ContractStatus = iota
	StatusPendingApproval
	StatusApproved
	StatusActive
	StatusVerified
	StatusFailed
	StatusEvolved
	StatusArchived
)

// String returns the string representation of a ContractStatus.
func (s ContractStatus) String() string {
	switch s {
	case StatusDraft:
		return "draft"
	case StatusPendingApproval:
		return "pending_approval"
	case StatusApproved:
		return "approved"
	case StatusActive:
		return "active"
	case StatusVerified:
		return "verified"
	case StatusFailed:
		return "failed"
	case StatusEvolved:
		return "evolved"
	case StatusArchived:
		return "archived"
	default:
		return "unknown"
	}
}

// StatusFromString parses a string into a ContractStatus.
func StatusFromString(s string) ContractStatus {
	switch s {
	case "draft":
		return StatusDraft
	case "pending_approval":
		return StatusPendingApproval
	case "approved":
		return StatusApproved
	case "active":
		return StatusActive
	case "verified":
		return StatusVerified
	case "failed":
		return StatusFailed
	case "evolved":
		return StatusEvolved
	case "archived":
		return StatusArchived
	default:
		return StatusDraft
	}
}

// Contract represents an executable declaration of intent.
type Contract struct {
	ID      string         `yaml:"id"`
	Name    string         `yaml:"name"`
	Version int            `yaml:"version"`
	Status  ContractStatus `yaml:"status"`

	// Four core elements
	Intent     string      `yaml:"intent"`
	Boundaries []Boundary  `yaml:"boundaries,omitempty"`
	Invariants []Invariant `yaml:"invariants,omitempty"`
	Examples   []Example   `yaml:"examples,omitempty"`

	// Hierarchy
	ParentID string   `yaml:"parent_id,omitempty"`
	ChildIDs []string `yaml:"child_ids,omitempty"`

	// Metadata
	CreatedAt  time.Time `yaml:"created_at"`
	UpdatedAt  time.Time `yaml:"updated_at"`
	ApprovedAt time.Time `yaml:"approved_at,omitempty"`
	CreatedBy  string    `yaml:"created_by,omitempty"`

	// Knowledge
	Lessons []Lesson `yaml:"lessons,omitempty"`

	// Verification
	LastVerification *VerificationResult `yaml:"last_verification,omitempty"`
}

// Boundary defines an input/output constraint.
type Boundary struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "input", "output", "side_effect"
	Description string `yaml:"description"`
	Constraint  string `yaml:"constraint,omitempty"`
}

// Invariant defines a condition that must always hold.
type Invariant struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Type        string `yaml:"type"` // "always", "never", "pre", "post"
	Check       string `yaml:"check,omitempty"`
	Severity    string `yaml:"severity,omitempty"` // "error", "warning"
}

// Example defines an input/output pair for verification.
type Example struct {
	Name           string `yaml:"name"`
	Description    string `yaml:"description,omitempty"`
	Input          string `yaml:"input,omitempty"`
	ExpectedOutput string `yaml:"expected_output,omitempty"`
	MatchType      string `yaml:"match_type,omitempty"` // "exact", "contains", "regex", "exit_code"
	Command        string `yaml:"command,omitempty"`
}

// Lesson represents accumulated knowledge from contract execution.
type Lesson struct {
	ID         string   `yaml:"id"`
	Content    string   `yaml:"content"`
	Category   string   `yaml:"category"` // "pitfall", "pattern", "optimization"
	ContractID string   `yaml:"contract_id"`
	Applicable []string `yaml:"applicable,omitempty"`
}

// VerificationResult holds the outcome of verifying a contract.
type VerificationResult struct {
	ContractID      string           `yaml:"contract_id"`
	Passed          bool             `yaml:"passed"`
	ExampleResults  []ExampleResult  `yaml:"example_results,omitempty"`
	InvariantResults []InvariantResult `yaml:"invariant_results,omitempty"`
	Duration        time.Duration    `yaml:"duration"`
	Summary         string           `yaml:"summary"`
	VerifiedAt      time.Time        `yaml:"verified_at"`
}

// ExampleResult holds the result of verifying a single example.
type ExampleResult struct {
	Name    string `yaml:"name"`
	Passed  bool   `yaml:"passed"`
	Output  string `yaml:"output,omitempty"`
	Error   string `yaml:"error,omitempty"`
}

// InvariantResult holds the result of checking a single invariant.
type InvariantResult struct {
	Name   string `yaml:"name"`
	Passed bool   `yaml:"passed"`
	Error  string `yaml:"error,omitempty"`
}

// ValidationError represents a contract validation error.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error in %s: %s", e.Field, e.Message)
}

// ValidationErrors collects multiple validation errors.
type ValidationErrors []ValidationError

func (e ValidationErrors) Error() string {
	if len(e) == 0 {
		return "no errors"
	}
	if len(e) == 1 {
		return e[0].Error()
	}
	var msgs []string
	for _, err := range e {
		msgs = append(msgs, err.Error())
	}
	return fmt.Sprintf("%d validation errors: %s", len(e), strings.Join(msgs, "; "))
}

// Valid boundary types
var validBoundaryTypes = map[string]bool{
	"input":       true,
	"output":      true,
	"side_effect": true,
	"invariant":   true, // A constraint that must remain constant
	"constraint":  true, // General constraint/limitation
}

// Valid invariant types
var validInvariantTypes = map[string]bool{
	"always": true,
	"never":  true,
	"pre":    true,
	"post":   true,
}

// Valid match types for examples
var validMatchTypes = map[string]bool{
	"":         true, // default (contains)
	"exact":    true,
	"contains": true,
	"regex":    true,
	"exit_code": true,
}

// Valid invariant severities
var validSeverities = map[string]bool{
	"":        true, // default (error)
	"error":   true,
	"warning": true,
}

// Validate checks the contract for structural validity.
// Returns nil if valid, ValidationErrors if there are issues.
func (c *Contract) Validate() error {
	var errors ValidationErrors

	// Required fields
	if c.ID == "" {
		errors = append(errors, ValidationError{Field: "id", Message: "is required"})
	}
	// Name is required, but we'll use ID as default if not provided
	if c.Name == "" {
		if c.ID != "" {
			c.Name = c.ID // Auto-fill from ID
		} else {
			errors = append(errors, ValidationError{Field: "name", Message: "is required"})
		}
	}
	if c.Intent == "" {
		errors = append(errors, ValidationError{Field: "intent", Message: "is required"})
	}
	if c.Version < 1 {
		errors = append(errors, ValidationError{Field: "version", Message: "must be >= 1"})
	}

	// Validate boundaries
	for i, b := range c.Boundaries {
		if b.Name == "" {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("boundaries[%d].name", i),
				Message: "is required",
			})
		}
		if b.Type != "" && !validBoundaryTypes[b.Type] {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("boundaries[%d].type", i),
				Message: fmt.Sprintf("invalid type '%s', must be one of: input, output, side_effect, invariant, constraint", b.Type),
			})
		}
		if b.Description == "" {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("boundaries[%d].description", i),
				Message: "is required",
			})
		}
	}

	// Validate invariants
	for i, inv := range c.Invariants {
		if inv.Name == "" {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("invariants[%d].name", i),
				Message: "is required",
			})
		}
		if inv.Type != "" && !validInvariantTypes[inv.Type] {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("invariants[%d].type", i),
				Message: fmt.Sprintf("invalid type '%s', must be one of: always, never, pre, post", inv.Type),
			})
		}
		if inv.Severity != "" && !validSeverities[inv.Severity] {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("invariants[%d].severity", i),
				Message: fmt.Sprintf("invalid severity '%s', must be one of: error, warning", inv.Severity),
			})
		}
		if inv.Description == "" {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("invariants[%d].description", i),
				Message: "is required",
			})
		}
	}

	// Validate examples
	for i, ex := range c.Examples {
		if ex.Name == "" {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("examples[%d].name", i),
				Message: "is required",
			})
		}
		if ex.MatchType != "" && !validMatchTypes[ex.MatchType] {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("examples[%d].match_type", i),
				Message: fmt.Sprintf("invalid match_type '%s', must be one of: exact, contains, regex, exit_code", ex.MatchType),
			})
		}
		// Warn if example has no command (can't be verified automatically)
		// This is not an error, just a soft warning tracked separately
	}

	// Validate lessons
	for i, l := range c.Lessons {
		if l.Content == "" {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("lessons[%d].content", i),
				Message: "is required",
			})
		}
		validCategories := map[string]bool{"pitfall": true, "pattern": true, "optimization": true, "": true}
		if !validCategories[l.Category] {
			errors = append(errors, ValidationError{
				Field:   fmt.Sprintf("lessons[%d].category", i),
				Message: fmt.Sprintf("invalid category '%s', must be one of: pitfall, pattern, optimization", l.Category),
			})
		}
	}

	if len(errors) > 0 {
		return errors
	}
	return nil
}

// HasVerifiableExamples returns true if the contract has examples with commands.
func (c *Contract) HasVerifiableExamples() bool {
	for _, ex := range c.Examples {
		if ex.Command != "" {
			return true
		}
	}
	return false
}

// HasVerifiableInvariants returns true if the contract has invariants with check commands.
func (c *Contract) HasVerifiableInvariants() bool {
	for _, inv := range c.Invariants {
		if inv.Check != "" {
			return true
		}
	}
	return false
}

// IsVerifiable returns true if the contract can be automatically verified.
func (c *Contract) IsVerifiable() bool {
	return c.HasVerifiableExamples() || c.HasVerifiableInvariants()
}

// FormatForContext renders the contract as human+AI-readable text for context injection.
func (c *Contract) FormatForContext() string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("Contract: %s (v%d) [%s]\n", c.Name, c.Version, c.Status.String()))
	sb.WriteString(fmt.Sprintf("Intent: %s\n", c.Intent))

	if len(c.Boundaries) > 0 {
		sb.WriteString("\nBoundaries:\n")
		for _, b := range c.Boundaries {
			sb.WriteString(fmt.Sprintf("  - [%s] %s: %s", b.Type, b.Name, b.Description))
			if b.Constraint != "" {
				sb.WriteString(fmt.Sprintf(" (constraint: %s)", b.Constraint))
			}
			sb.WriteString("\n")
		}
	}

	if len(c.Invariants) > 0 {
		sb.WriteString("\nInvariants:\n")
		for _, inv := range c.Invariants {
			sb.WriteString(fmt.Sprintf("  - [%s] %s: %s\n", inv.Type, inv.Name, inv.Description))
		}
	}

	if len(c.Examples) > 0 {
		sb.WriteString("\nExamples:\n")
		for _, ex := range c.Examples {
			sb.WriteString(fmt.Sprintf("  - %s", ex.Name))
			if ex.Input != "" {
				sb.WriteString(fmt.Sprintf(": %s", ex.Input))
			}
			if ex.ExpectedOutput != "" {
				sb.WriteString(fmt.Sprintf(" -> %s", ex.ExpectedOutput))
			}
			sb.WriteString("\n")
		}
	}

	if len(c.Lessons) > 0 {
		sb.WriteString("\nLessons:\n")
		for _, l := range c.Lessons {
			sb.WriteString(fmt.Sprintf("  - [%s] %s\n", l.Category, l.Content))
		}
	}

	return sb.String()
}

// FormatForDisplay renders the contract with ANSI colors for TUI display.
func (c *Contract) FormatForDisplay() string {
	const (
		reset  = "\033[0m"
		bold   = "\033[1m"
		green  = "\033[32m"
		yellow = "\033[33m"
		blue   = "\033[34m"
		cyan   = "\033[36m"
	)

	var sb strings.Builder

	// Header
	sb.WriteString(fmt.Sprintf("%s%s%s (v%d) [%s%s%s]\n",
		bold, c.Name, reset, c.Version, yellow, c.Status.String(), reset))
	sb.WriteString(fmt.Sprintf("%sIntent:%s %s\n", cyan, reset, c.Intent))

	if len(c.Boundaries) > 0 {
		sb.WriteString(fmt.Sprintf("\n%sBoundaries:%s\n", blue, reset))
		for _, b := range c.Boundaries {
			sb.WriteString(fmt.Sprintf("  %s[%s]%s %s: %s\n", green, b.Type, reset, b.Name, b.Description))
		}
	}

	if len(c.Invariants) > 0 {
		sb.WriteString(fmt.Sprintf("\n%sInvariants:%s\n", blue, reset))
		for _, inv := range c.Invariants {
			sb.WriteString(fmt.Sprintf("  %s[%s]%s %s: %s\n", green, inv.Type, reset, inv.Name, inv.Description))
		}
	}

	if len(c.Examples) > 0 {
		sb.WriteString(fmt.Sprintf("\n%sExamples:%s\n", blue, reset))
		for _, ex := range c.Examples {
			sb.WriteString(fmt.Sprintf("  %s%s%s", green, ex.Name, reset))
			if ex.Input != "" {
				sb.WriteString(fmt.Sprintf(": %s", ex.Input))
			}
			if ex.ExpectedOutput != "" {
				sb.WriteString(fmt.Sprintf(" -> %s", ex.ExpectedOutput))
			}
			sb.WriteString("\n")
		}
	}

	if c.LastVerification != nil {
		status := "PASSED"
		color := green
		if !c.LastVerification.Passed {
			status = "FAILED"
			color = "\033[31m" // red
		}
		sb.WriteString(fmt.Sprintf("\n%sLast Verification:%s %s%s%s (%s)\n",
			blue, reset, color, status, reset, c.LastVerification.Summary))
	}

	return sb.String()
}
