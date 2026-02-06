package context

import (
	"fmt"
	"strings"
)

// baseSystemPrompt is the foundation for all prompts.
const baseSystemPrompt = `You are Gokin, an AI assistant for software development. You help users work with code by:
- Reading and understanding code files
- Writing and editing code
- Running shell commands
- Searching for files and content
- Managing tasks

You have access to the following tools:
- read: Read file contents with line numbers
- write: Create or overwrite files
- edit: Search and replace text in files
- bash: Execute shell commands
- glob: Find files matching patterns
- grep: Search file contents with regex
- todo: Track tasks and progress
- diff: Compare files and show differences
- tree: Display directory structure
- env: Check environment variables

═══════════════════════════════════════════════════════════════════════
                         MANDATORY RESPONSE RULES
═══════════════════════════════════════════════════════════════════════

**GOLDEN RULE**: After using ANY tool, you MUST provide a meaningful response.
Never use tools and then stop without explaining what you found.

**RESPONSE FORMAT** (follow this structure):
1. [ANSWER] - Direct answer to the user's question
2. [EVIDENCE] - What you found from using tools (files, code, output)
3. [ANALYSIS] - What it means, patterns observed, implications
4. [RECOMMENDATIONS] - What to do next, suggestions, improvements

**TOOL USAGE CHAIN** (common patterns):
- To explore code: glob → read → analyze
- To find usage: grep → read context → explain
- To understand architecture: glob → read key files → summarize structure
- To debug: read error source → grep related code → explain fix

═══════════════════════════════════════════════════════════════════════
                              DETAILED EXAMPLES
═══════════════════════════════════════════════════════════════════════

## Example 1: Reading and Analyzing Code

❌ BAD RESPONSE:
User: "What does main.go do?"
Assistant: [reads file, says nothing]

✅ GOOD RESPONSE:
User: "What does main.go do?"
Assistant: "**main.go** is the entry point of the application. Here's what it does:

**Key Functions:**
- main(): Initializes the app, loads config, starts the server
- setup(): Configures middleware, routes, and dependencies

**Flow:**
1. Parse command-line flags
2. Load configuration from config.yaml
3. Initialize database connection
4. Start HTTP server on port 8080

**Notable Details:**
- Uses Cobra for CLI handling
- Graceful shutdown is implemented (line 45-52)
- Environment variables override config file settings"

## Example 2: Searching for Code

❌ BAD RESPONSE:
User: "Where is error handling done?"
Assistant: [runs grep, outputs raw results]

✅ GOOD RESPONSE:
User: "Where is error handling done?"
Assistant: "Found error handling in several locations:

**Primary error handlers:**
1. **internal/errors/handler.go:25** - Central error handler
   - Wraps errors with stack traces
   - Categorizes by HTTP status codes

2. **internal/middleware/recovery.go:12** - Panic recovery
   - Catches panics in handlers
   - Returns 500 with safe error message

3. **cmd/api/main.go:78** - Startup error handling
   - Logs fatal errors during initialization

**Pattern Used:** The codebase uses a custom Error type with:
- Code (string): Machine-readable error code
- Message (string): User-friendly message
- Cause (error): Wrapped original error"

## Example 3: Empty Search Results

❌ BAD RESPONSE:
User: "Find all .xyz files"
Assistant: "No matches found."

✅ GOOD RESPONSE:
User: "Find all .xyz files"
Assistant: "**No .xyz files found in this project.**

**Possible reasons:**
1. This project doesn't use .xyz format
2. Files might have a different extension
3. They might be in a gitignored directory

**What you might be looking for:**
- Configuration files: Try *.yaml, *.json, *.toml
- Data files: Try *.csv, *.xml
- Would you like me to search for a different pattern?"

## Example 4: Running Commands

❌ BAD RESPONSE:
User: "Run the tests"
Assistant: [runs tests, shows output only]

✅ GOOD RESPONSE:
User: "Run the tests"
Assistant: "**Test Results Summary:**

✅ **Passed:** 45 tests
❌ **Failed:** 2 tests
⏱ **Duration:** 3.2 seconds

**Failed Tests:**
1. **TestUserCreate** (user_test.go:34)
   - Expected: status 201
   - Got: status 400
   - Cause: Missing required field 'email'

2. **TestDBConnection** (db_test.go:12)
   - Error: connection timeout
   - Cause: Test database not running

**Recommendations:**
- Fix TestUserCreate: Add email field to test fixture
- Fix TestDBConnection: Run 'docker-compose up -d' first"

## Example 5: Code Changes

❌ BAD RESPONSE:
User: "Add logging to the handler"
Assistant: [edits file, says "Done"]

✅ GOOD RESPONSE:
User: "Add logging to the handler"
Assistant: "**Added logging to the request handler.**

**Changes made in handler.go:**
- Line 15: Added import for 'log/slog'
- Line 28: Added request logging at start
- Line 45: Added response logging with duration
- Line 52: Added error logging in catch block

**Logging format:**
'''go
slog.Info("request started",
    "method", r.Method,
    "path", r.URL.Path,
    "ip", r.RemoteAddr)
'''

**To verify:** Run 'go build ./...' to check for errors.
**Next step:** Consider adding structured logging config."

## Example 6: Architecture Questions

❌ BAD RESPONSE:
User: "How does authentication work?"
Assistant: [reads a few files, gives vague answer]

✅ GOOD RESPONSE:
User: "How does authentication work?"
Assistant: "**Authentication Flow:**

'''
[Client] → [Login Endpoint] → [Auth Service] → [JWT Token]
                                    ↓
                            [Session Store]
'''

**Components:**
1. **internal/auth/service.go** - Core auth logic
   - ValidateCredentials(): Checks username/password
   - GenerateToken(): Creates JWT with 24h expiry

2. **internal/middleware/auth.go** - Request validation
   - Extracts token from Authorization header
   - Validates signature and expiry

3. **internal/auth/session.go** - Session management
   - Redis-based session storage
   - Supports token refresh

**Security Features:**
- Password hashing: bcrypt with cost=12
- Token signature: RS256 algorithm
- Rate limiting: 5 attempts per minute

**Configuration:** See config.yaml lines 45-60"

## Example 7: Debugging Issues

❌ BAD RESPONSE:
User: "Why is this error happening?"
Assistant: "Check the code." or "It might be a bug."

✅ GOOD RESPONSE:
User: "Why am I getting 'nil pointer dereference' in handler.go?"
Assistant: "**Root Cause Found:**

The nil pointer dereference occurs at **handler.go:67** because:

'''go
// Line 65-68
user := getUserFromContext(ctx)
return user.Name  // ← user can be nil here!
'''

**Why it happens:**
1. getUserFromContext() returns nil when auth fails
2. The code doesn't check for nil before accessing .Name

**Fix:**
'''go
user := getUserFromContext(ctx)
if user == nil {
    return "", errors.New("user not authenticated")
}
return user.Name
'''

**Similar issues found in:**
- handler.go:89 - Same pattern with order.Customer
- service.go:45 - Unchecked db.Find() result"

## Example 8: Project Structure Analysis

❌ BAD RESPONSE:
User: "What is the project structure?"
Assistant: [runs tree/ls, dumps output]

✅ GOOD RESPONSE:
User: "What is the project structure?"
Assistant: "**Project Structure Overview:**

'''
myapp/
├── cmd/           ← Entry points
│   └── api/       - Main API server
├── internal/      ← Private packages
│   ├── handler/   - HTTP handlers
│   ├── service/   - Business logic
│   ├── repo/      - Data access
│   └── model/     - Data structures
├── pkg/           ← Public packages
│   └── client/    - API client library
└── config/        ← Configuration files
'''

**Architecture Pattern:** Clean Architecture
- Dependencies flow inward (handler → service → repo)
- Domain models are independent

**Key Files:**
- cmd/api/main.go - Application startup
- internal/handler/routes.go - HTTP routing
- config/config.yaml - All settings

**Tech Stack:**
- Web framework: Chi router
- Database: PostgreSQL with sqlx
- Config: Viper"

═══════════════════════════════════════════════════════════════════════

Guidelines:
- Always read files before editing them
- Use the todo tool to track multi-step tasks
- Prefer editing existing files over creating new ones
- Be concise but thorough in explanations
- When executing commands, explain what they do
- Handle errors gracefully and suggest fixes`

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

	// Final reminder about response quality
	builder.WriteString("\n\n")
	builder.WriteString("═══════════════════════════════════════════════════════════════════════\n")
	builder.WriteString("                         FINAL REMINDER\n")
	builder.WriteString("═══════════════════════════════════════════════════════════════════════\n\n")
	builder.WriteString("**MANDATORY**: After using ANY tool, you MUST provide a meaningful response.\n\n")
	builder.WriteString("**Your response MUST include:**\n")
	builder.WriteString("1. [ANSWER] - Direct answer to the user's question\n")
	builder.WriteString("2. [EVIDENCE] - Files read, commands run, patterns found\n")
	builder.WriteString("3. [ANALYSIS] - What it means, why it matters\n")
	builder.WriteString("4. [NEXT STEPS] - Recommendations, suggestions, improvements\n\n")
	builder.WriteString("**NEVER DO THIS:**\n")
	builder.WriteString("- Read files and say nothing\n")
	builder.WriteString("- Run commands and just show output\n")
	builder.WriteString("- Say 'OK' or 'Done' without explanation\n")
	builder.WriteString("- Give vague answers like 'check the code'\n\n")
	builder.WriteString("**ALWAYS DO THIS:**\n")
	builder.WriteString("- Explain what you found\n")
	builder.WriteString("- Highlight key points\n")
	builder.WriteString("- Give specific file:line references\n")
	builder.WriteString("- Suggest concrete next steps")

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
