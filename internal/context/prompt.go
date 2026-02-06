package context

import (
	"fmt"
	"strings"
)

// baseSystemPrompt is the foundation for all prompts.
const baseSystemPrompt = `You are Gokin, an AI coding assistant. You help users work with code through tools for reading, writing, searching, and executing commands.

## Tool Selection (ALWAYS prefer dedicated tools over bash)

- Find files → glob (NOT bash find/ls)
- Search content → grep (NOT bash grep/rg)
- Read file → read (NOT bash cat/head/tail)
- Targeted edit → edit (NOT write entire file)
- New file → write
- Run commands/builds/tests → bash (only when no dedicated tool exists)

When multiple independent operations are needed, call tools in parallel.

## After Using Tools

1. Explain what you found — specific files, lines, patterns
2. Analyze meaning — why it matters for the user's task
3. Suggest next steps — concrete actions

Never: dump raw output without analysis, say "Done" without explanation, give vague answers.

## Security

- Never run destructive commands (rm -rf /, drop database) without explicit user confirmation
- Don't commit secrets (.env, API keys, credentials)
- Don't introduce injection vulnerabilities (SQL, XSS, command injection)
- Read files before editing — understand context first
- Prefer edit over write for existing files to avoid data loss

## Git Workflow

- Check git_status before committing
- Review changes with git_diff before commit
- Never force push without explicit user request
- Write descriptive commit messages explaining WHY, not just WHAT
- Stage specific files, not everything blindly

## Response Style

- Be concise but thorough — explain what matters, skip what doesn't
- Use file:line references for specific code locations
- When referencing code, show the relevant snippet
- For multi-step tasks, use todo to track progress
- Handle errors gracefully and suggest fixes

## Example

User: "Where is error handling done?"

Good response after using grep:
"Found error handling in 12 locations across 5 files:

**Core handlers:**
- internal/errors/handler.go:25 — Central error handler, wraps with stack traces
- internal/middleware/recovery.go:12 — Panic recovery middleware

**Usage in API:**
- api/users.go:45, 67, 89 — User endpoint errors
- api/orders.go:34 — Order validation

Pattern: All errors are wrapped with context before returning to the caller."

Guidelines:
- Always read files before editing them
- Prefer editing existing files over creating new ones
- When executing commands, explain what they do`

// legacyPlanInstructions is used when auto-detect planning is disabled.
const legacyPlanInstructions = `
Plan Mode:
- When a user provides feedback on a plan (after pressing ESC or requesting changes), you MUST:
  1. First call get_plan_status to check if there's a previously rejected plan
  2. If there is, review the rejected plan carefully
  3. Address the user's specific feedback
  4. Create a NEW plan using enter_plan_mode that incorporates their feedback
  5. Do NOT ignore the previous plan - build upon it and make the requested changes
`

// autoPlanningProtocol is the comprehensive planning protocol used when auto-detect is enabled.
const autoPlanningProtocol = `
═══════════════════════════════════════════════════════════════════════
                    AUTOMATIC PLANNING PROTOCOL
═══════════════════════════════════════════════════════════════════════

You MUST automatically enter plan mode when the user's request meets ANY of these criteria:
- Implementation or feature request (adding new functionality)
- Multi-file changes (refactoring, renaming, migrating)
- Bug fixes requiring understanding multiple components
- Architecture or design changes
- Any task requiring more than 3 sequential tool calls
- Any task where getting it wrong would require significant undo

You MUST NOT enter plan mode for:
- Simple questions about code
- Single-file reads or searches
- Running a single command
- Explaining concepts

PLANNING WORKFLOW (MANDATORY):

PHASE 1: EXPLORATION
1. Use glob to understand project structure
2. Use grep to find relevant patterns/usages
3. Use read to examine key files
4. Identify dependencies, interfaces, existing patterns
5. NEVER skip exploration

PHASE 2: PLAN CREATION
Call enter_plan_mode with:
- Clear, specific title
- Description explaining approach and WHY
- Concrete steps mapping to specific file changes
- Steps ordered by dependency
- Include verification steps (tests, builds)

PHASE 3: WAIT FOR APPROVAL
Tool blocks until user approves, rejects, or requests modifications.

PHASE 4: EXECUTION
- For EACH step: call update_plan_progress with action "start"
- Execute the step's work
- Call update_plan_progress with action "complete"
- After all steps: call exit_plan_mode with reason "completed"

PLAN FEEDBACK HANDLING:
When a user provides feedback on a plan (after pressing ESC or requesting changes), you MUST:
1. Call get_plan_status to check the rejected plan
2. Review the rejected plan carefully
3. Address the user's specific feedback point by point
4. Create a NEW plan using enter_plan_mode incorporating feedback
5. Explain what changed from the previous plan
Do NOT ignore the previous plan - build upon it and make the requested changes.

STEP QUALITY:
- Specific: "Add validateEmail function to internal/auth/validator.go" not "add validation"
- Atomic: One logical change per step
- Verifiable: Include HOW to verify the step worked
- Ordered: Dependencies before dependents

CONTRACT-FIRST PLANNING:
For tasks with clear I/O (functions, APIs, endpoints): fill contract fields in enter_plan_mode.
For exploratory/refactoring tasks: skip contract fields.

Example - "Add email validation function":
  contract_name: "email_validator"
  intent: "Validate email format per RFC 5322 and return bool"
  boundaries: [{type: "input", name: "email", description: "email address string"}]
  invariants: [{type: "always", name: "rfc5322", description: "RFC 5322 compliant addresses return true"}]
  examples: [{name: "valid_email", command: "go test ./... -run TestEmailValid", expected_output: "PASS", match_type: "contains"}]
  steps: [{title: "Create validator", description: "Add ValidateEmail to internal/auth/"}]

═══════════════════════════════════════════════════════════════════════
`

// projectGuidelines contains project-type-specific guidelines.
var projectGuidelines = map[ProjectType]string{
	ProjectTypeGo: `
## Go Project Guidelines
- Use 'go mod tidy' after adding or removing dependencies
- Run 'go test ./...' to run all tests
- Use 'go vet ./...' for static analysis
- Follow Go naming conventions (camelCase for private, PascalCase for exported)
- Run 'go fmt' to format code
- Prefer 'go build ./...' to verify compilation
- Use meaningful error messages with context`,

	ProjectTypeNode: `
## Node.js Project Guidelines
- Check package.json scripts before running commands
- Use the detected package manager (%s)
- Run '%s install' to install dependencies
- Run '%s test' for testing
- Check for .nvmrc or .node-version for Node version
- Prefer ES modules if using "type": "module"`,

	ProjectTypeRust: `
## Rust Project Guidelines
- Use 'cargo build' to compile
- Use 'cargo test' to run tests
- Use 'cargo fmt' to format code
- Use 'cargo clippy' for linting
- Check Cargo.toml for dependencies and features
- Prefer Result<T, E> for error handling`,

	ProjectTypePython: `
## Python Project Guidelines
- Use the detected package manager (%s)
- Check for virtual environment (.venv, venv)
- Use 'pytest' or detected test framework for testing
- Check pyproject.toml or setup.py for project config
- Follow PEP 8 style guidelines
- Use type hints where appropriate`,

	ProjectTypeJava: `
## Java Project Guidelines
- Use the detected build tool (Maven/Gradle)
- Check pom.xml or build.gradle for configuration
- Run tests with the build tool
- Follow Java naming conventions`,

	ProjectTypeRuby: `
## Ruby Project Guidelines
- Use 'bundle install' to install dependencies
- Check Gemfile for dependencies
- Use 'rake' or 'rspec' for testing
- Follow Ruby style guidelines`,

	ProjectTypePHP: `
## PHP Project Guidelines
- Use 'composer install' for dependencies
- Check composer.json for configuration
- Use PHPUnit for testing
- Follow PSR coding standards`,
}

// MemoryProvider provides memory entries for prompt injection.
type MemoryProvider interface {
	GetForContext(projectOnly bool) string
}

// PlanStepInfo holds step information for plan execution prompts.
type PlanStepInfo struct {
	ID          int
	Title       string
	Description string
}

// PlanManagerProvider provides active contract context from plan manager.
type PlanManagerProvider interface {
	GetActiveContractContext() string
}

// PromptBuilder builds dynamic system prompts.
type PromptBuilder struct {
	workDir         string
	projectInfo     *ProjectInfo
	projectMemory   *ProjectMemory
	memoryStore     MemoryProvider
	planAutoDetect  bool
	planManager     PlanManagerProvider
	detectedContext string // Auto-detected project context (frameworks, docs, etc.)
	toolHints       string // Tool usage pattern hints
}

// NewPromptBuilder creates a new prompt builder.
func NewPromptBuilder(workDir string, projectInfo *ProjectInfo) *PromptBuilder {
	return &PromptBuilder{
		workDir:     workDir,
		projectInfo: projectInfo,
	}
}

// SetProjectMemory sets the project memory for custom instructions.
func (b *PromptBuilder) SetProjectMemory(memory *ProjectMemory) {
	b.projectMemory = memory
}

// SetMemoryStore sets the memory store for persistent memory injection.
func (b *PromptBuilder) SetMemoryStore(store MemoryProvider) {
	b.memoryStore = store
}

// SetPlanAutoDetect enables or disables auto-planning in the system prompt.
func (b *PromptBuilder) SetPlanAutoDetect(enabled bool) {
	b.planAutoDetect = enabled
}

// SetPlanManager sets the plan manager for contract context injection.
func (b *PromptBuilder) SetPlanManager(pm PlanManagerProvider) {
	b.planManager = pm
}

// SetDetectedContext sets the auto-detected project context (frameworks, docs summaries).
func (b *PromptBuilder) SetDetectedContext(ctx string) {
	b.detectedContext = ctx
}

// SetToolHints sets the tool usage pattern hints for periodic injection.
func (b *PromptBuilder) SetToolHints(hints string) {
	b.toolHints = hints
}

// Build constructs the full system prompt.
func (b *PromptBuilder) Build() string {
	var builder strings.Builder

	// Base prompt
	builder.WriteString(baseSystemPrompt)
	builder.WriteString("\n")

	// Add project-specific guidelines
	if b.projectInfo != nil && b.projectInfo.Type != ProjectTypeUnknown {
		builder.WriteString(b.buildProjectSection())
	}

	// Add project memory instructions (from GOKIN.md)
	if b.projectMemory != nil && b.projectMemory.HasInstructions() {
		builder.WriteString("\n\n## Project Instructions\n")
		builder.WriteString("The following instructions are specific to this project:\n\n")
		builder.WriteString(b.projectMemory.GetInstructions())
	}

	// Add persistent memories from memory store
	if b.memoryStore != nil {
		memoryContent := b.memoryStore.GetForContext(true) // Project-specific memories
		if memoryContent != "" {
			builder.WriteString("\n\n")
			builder.WriteString(memoryContent)
		}
	}

	// Add working directory
	builder.WriteString(fmt.Sprintf("\n\nThe user's working directory is: %s", b.workDir))

	// Add project context if available
	if b.projectInfo != nil && b.projectInfo.Name != "" {
		builder.WriteString(fmt.Sprintf("\nProject name: %s", b.projectInfo.Name))
	}

	// Inject auto-detected project context (frameworks, doc summaries, dependencies)
	if b.detectedContext != "" {
		builder.WriteString("\n\n## Detected Project Context\n")
		builder.WriteString(b.detectedContext)
	}

	// Inject tool usage pattern hints (populated periodically)
	if b.toolHints != "" {
		builder.WriteString("\n\n## Tool Usage Hints\n")
		builder.WriteString(b.toolHints)
	}

	// Add plan mode instructions (conditional on auto-detect)
	if b.planAutoDetect {
		builder.WriteString(autoPlanningProtocol)
	} else {
		builder.WriteString(legacyPlanInstructions)
	}

	// Inject active contract context from plan manager
	if b.planManager != nil {
		if ctx := b.planManager.GetActiveContractContext(); ctx != "" {
			builder.WriteString("\n\n=== ACTIVE CONTRACT ===\n")
			builder.WriteString(ctx)
			builder.WriteString("\n=== END CONTRACT ===\n")
			builder.WriteString("\nOperate strictly within this contract's boundaries.\n")
		}
	}

	// Add tool chain patterns for common tasks
	builder.WriteString("\n\n## Common Task Patterns\n")
	for name, pattern := range ToolChainPatterns {
		builder.WriteString(fmt.Sprintf("**%s:** %s\n\n", name, pattern))
	}

	return builder.String()
}

// buildProjectSection builds the project-specific section.
func (b *PromptBuilder) buildProjectSection() string {
	guidelines, ok := projectGuidelines[b.projectInfo.Type]
	if !ok {
		return ""
	}

	// Format guidelines with project-specific info
	switch b.projectInfo.Type {
	case ProjectTypeNode:
		pm := b.projectInfo.PackageManager
		if pm == "" {
			pm = "npm"
		}
		guidelines = fmt.Sprintf(guidelines, pm, pm, pm)
	case ProjectTypePython:
		pm := b.projectInfo.PackageManager
		if pm == "" {
			pm = "pip"
		}
		guidelines = fmt.Sprintf(guidelines, pm)
	}

	var builder strings.Builder
	builder.WriteString(guidelines)

	// Add detected details
	if len(b.projectInfo.Dependencies) > 0 {
		builder.WriteString(fmt.Sprintf("\n\nKey dependencies: %s", strings.Join(b.projectInfo.Dependencies, ", ")))
	}

	if b.projectInfo.TestFramework != "" {
		builder.WriteString(fmt.Sprintf("\nTest command: %s", b.projectInfo.TestFramework))
	}

	if b.projectInfo.BuildTool != "" {
		builder.WriteString(fmt.Sprintf("\nBuild tool: %s", b.projectInfo.BuildTool))
	}

	return builder.String()
}

// BuildWithContext builds a prompt with additional context.
func (b *PromptBuilder) BuildWithContext(additionalContext string) string {
	base := b.Build()
	if additionalContext == "" {
		return base
	}

	return base + "\n\nAdditional context:\n" + additionalContext
}

// GetProjectSummary returns a brief summary of the detected project.
func (b *PromptBuilder) GetProjectSummary() string {
	if b.projectInfo == nil || b.projectInfo.Type == ProjectTypeUnknown {
		return ""
	}

	parts := []string{b.projectInfo.Type.String()}

	if b.projectInfo.Name != "" {
		parts = append(parts, fmt.Sprintf("(%s)", b.projectInfo.Name))
	}

	if b.projectInfo.PackageManager != "" {
		parts = append(parts, fmt.Sprintf("[%s]", b.projectInfo.PackageManager))
	}

	return strings.Join(parts, " ")
}

// BuildPlanExecutionPrompt constructs a minimal focused prompt for executing an approved plan.
// This prompt is used after context is cleared, providing only what's needed for plan execution.
func (b *PromptBuilder) BuildPlanExecutionPrompt(title, description string, steps []PlanStepInfo) string {
	var builder strings.Builder

	builder.WriteString("You are Gokin, executing an approved plan. Execute precisely.\n\n")

	// Add project-specific guidelines (reuse existing method)
	if b.projectInfo != nil && b.projectInfo.Type != ProjectTypeUnknown {
		builder.WriteString(b.buildProjectSection())
		builder.WriteString("\n\n")
	}

	// Add project instructions (from GOKIN.md)
	if b.projectMemory != nil && b.projectMemory.HasInstructions() {
		builder.WriteString("## Project Instructions\n")
		builder.WriteString(b.projectMemory.GetInstructions())
		builder.WriteString("\n\n")
	}

	// Add persistent memories from memory store (critical for context retention)
	if b.memoryStore != nil {
		memoryContent := b.memoryStore.GetForContext(true)
		if memoryContent != "" {
			builder.WriteString("## Project Knowledge\n")
			builder.WriteString(memoryContent)
			builder.WriteString("\n\n")
		}
	}

	// Inject active contract context from plan manager
	if b.planManager != nil {
		if ctx := b.planManager.GetActiveContractContext(); ctx != "" {
			builder.WriteString("## Active Contract\n")
			builder.WriteString(ctx)
			builder.WriteString("\n\n")
		}
	}

	// The approved plan
	builder.WriteString("═══════════════════════════════════════════════════════════════════════\n")
	builder.WriteString("                         APPROVED PLAN\n")
	builder.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
	builder.WriteString(fmt.Sprintf("## %s\n", title))
	if description != "" {
		builder.WriteString(fmt.Sprintf("%s\n", description))
	}
	builder.WriteString("\n### Steps:\n")
	for _, step := range steps {
		builder.WriteString(fmt.Sprintf("%d. **%s**\n", step.ID, step.Title))
		if step.Description != "" {
			builder.WriteString(fmt.Sprintf("   %s\n", step.Description))
		}
	}

	// Execution rules
	builder.WriteString("\n═══════════════════════════════════════════════════════════════════════\n")
	builder.WriteString("                       EXECUTION RULES\n")
	builder.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
	builder.WriteString("1. For EACH step: call update_plan_progress with action \"start\" BEFORE doing work\n")
	builder.WriteString("2. Execute the step's work thoroughly\n")
	builder.WriteString("3. Call update_plan_progress with action \"complete\" after finishing\n")
	builder.WriteString("4. Always READ files before editing them\n")
	builder.WriteString("5. Do NOT deviate from the plan - execute exactly what was approved\n")
	builder.WriteString("6. If a step fails, call update_plan_progress with action \"fail\" and continue to next step\n")
	builder.WriteString("7. After ALL steps: call exit_plan_mode with reason \"completed\"\n")
	builder.WriteString("8. Provide brief status updates after each step\n")

	// Working directory
	builder.WriteString(fmt.Sprintf("\nWorking directory: %s\n", b.workDir))

	return builder.String()
}

// BuildPlanExecutionPromptWithContext is like BuildPlanExecutionPrompt but includes
// a context snapshot from the planning conversation to preserve important decisions.
func (b *PromptBuilder) BuildPlanExecutionPromptWithContext(title, description string, steps []PlanStepInfo, contextSnapshot string) string {
	basePrompt := b.BuildPlanExecutionPrompt(title, description, steps)

	if contextSnapshot == "" {
		return basePrompt
	}

	// Insert context snapshot before the approved plan section
	var builder strings.Builder
	builder.WriteString("You are Gokin, executing an approved plan. Execute precisely.\n\n")

	// Context from planning discussion (important decisions, findings)
	builder.WriteString(contextSnapshot)
	builder.WriteString("\n")

	// The rest is from base prompt (skip the first line which we already wrote)
	lines := strings.SplitN(basePrompt, "\n", 2)
	if len(lines) > 1 {
		builder.WriteString(lines[1])
	}

	return builder.String()
}

// BuildSubAgentPrompt builds a compact project context for injection into sub-agents.
// Unlike Build(), this omits examples, response format rules, and planning protocol.
// It provides project guidelines, GOKIN.md instructions, memory, and working directory.
func (b *PromptBuilder) BuildSubAgentPrompt() string {
	var builder strings.Builder

	// Project-specific guidelines (Go, Node, Python, etc.)
	if b.projectInfo != nil && b.projectInfo.Type != ProjectTypeUnknown {
		builder.WriteString(b.buildProjectSection())
		builder.WriteString("\n")
	}

	// Project instructions from GOKIN.md
	if b.projectMemory != nil && b.projectMemory.HasInstructions() {
		builder.WriteString("\n## Project Instructions\n")
		builder.WriteString(b.projectMemory.GetInstructions())
		builder.WriteString("\n")
	}

	// Add persistent memories (project knowledge, decisions, etc.)
	if b.memoryStore != nil {
		memoryContent := b.memoryStore.GetForContext(true)
		if memoryContent != "" {
			builder.WriteString("\n## Project Knowledge\n")
			builder.WriteString(memoryContent)
			builder.WriteString("\n")
		}
	}

	// Inject active contract context
	if b.planManager != nil {
		if ctx := b.planManager.GetActiveContractContext(); ctx != "" {
			builder.WriteString("\n## Active Contract\n")
			builder.WriteString(ctx)
			builder.WriteString("\n")
		}
	}

	// Working directory
	builder.WriteString(fmt.Sprintf("\nWorking directory: %s\n", b.workDir))

	// Project name
	if b.projectInfo != nil && b.projectInfo.Name != "" {
		builder.WriteString(fmt.Sprintf("Project: %s\n", b.projectInfo.Name))
	}

	return builder.String()
}
