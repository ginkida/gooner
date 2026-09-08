package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gokin/internal/app"
	"gokin/internal/chat"
	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/logging"
	"gokin/internal/permission"
	"gokin/internal/security"
	"gokin/internal/setup"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	// version is the dev-time fallback. Release builds inject the real value
	// via `-X main.version=$(git describe --tags)` — see .github/workflows/release.yml.
	// Bump this when merging a sprint worth of changes so `go build` without
	// ldflags still shows something sensible in /version.
	version              = "0.100.141"
	cfgFile              string
	model                string
	provider             string
	baseURL              string
	runSetup             bool
	continueLast         bool
	resumeSession        string
	sessionID            string
	forkSession          bool
	backgroundMode       bool
	noSessionPersistence bool
	headless             bool
	printMode            bool
	prompt               string
	outputFormat         string
	inputFormat          string
	headlessTurns        int
	headlessLimit        time.Duration
	headlessBudget       float64
	permissionMode       string
	skipPermissions      bool
	toolCeiling          []string
	allowedTools         []string
	deniedTools          []string
	allowedToolsCompat   []string
	deniedToolsCompat    []string
	addDirs              []string
	systemPrompt         string
	systemPromptFile     string
	appendSystemPrompt   string
	appendSystemFile     string
	jsonSchema           string
	bare                 bool
	debugCategories      string
	debugFile            string
	rootInvocationArgs   []string
)

const maxHeadlessStdinBytes = 16 << 20

// rootLongHelp is the first thing a new user reads, and it names the default
// provider — which is to say it tells them which API key to go and obtain. It
// is a const rather than an inline literal so a test can hold it against
// config.DefaultConfig: the claim went stale once already, still naming Kimi
// after the default moved to GLM.
const rootLongHelp = `Gokin is a CLI tool for AI-assisted coding. Supports GLM
(default), Kimi, DeepSeek, MiniMax, and Ollama. It provides an
interactive chat interface with tools for reading, writing, and
editing files, running commands, and orchestrating multi-agent
workflows — with zero proxies between you and the provider you
choose.`

func main() {
	rootCmd := &cobra.Command{
		Use:          "gokin",
		Short:        "AI-powered CLI assistant for code",
		Long:         rootLongHelp,
		Args:         cobra.ArbitraryArgs,
		RunE:         runApp,
		SilenceUsage: true,
		Version:      version,
	}

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/gokin/config.yaml)")
	rootCmd.PersistentFlags().StringVar(&model, "model", "", "model to use (default depends on provider)")
	rootCmd.PersistentFlags().StringVar(&provider, "provider", "", "provider to use for this run (glm, minimax, kimi, deepseek, ollama)")
	rootCmd.PersistentFlags().StringVar(&baseURL, "base-url", "", "custom provider API base URL for this run (in-memory only)")
	rootCmd.PersistentFlags().BoolVar(&runSetup, "setup", false, "run the setup wizard")
	rootCmd.PersistentFlags().BoolVarP(&continueLast, "continue", "c", false, "continue the most recent session in this workspace")
	rootCmd.PersistentFlags().StringVarP(&resumeSession, "resume", "r", "", "resume an exact session ID or saved name")
	rootCmd.PersistentFlags().StringVar(&sessionID, "session-id", "", "use a specific UUID for a new session")
	rootCmd.PersistentFlags().BoolVar(&forkSession, "fork-session", false, "resume into a new session ID instead of modifying the source")
	rootCmd.PersistentFlags().BoolVar(&backgroundMode, "background", false, "start as a detached session and return immediately")
	rootCmd.PersistentFlags().BoolVar(&backgroundMode, "bg", false, "start as a detached session and return immediately")
	rootCmd.PersistentFlags().BoolVar(&noSessionPersistence, "no-session-persistence", false, "do not save or auto-load session state for this process")
	rootCmd.PersistentFlags().BoolVar(&headless, "headless", false, "run one prompt without the interactive TUI")
	rootCmd.PersistentFlags().BoolVarP(&printMode, "print", "p", false, "run one prompt and exit (alias for --headless)")
	rootCmd.PersistentFlags().StringVar(&prompt, "prompt", "", "prompt to run initially (headless also accepts redirected stdin)")
	rootCmd.PersistentFlags().StringVar(&outputFormat, "output-format", "text", "headless output format: text, json, or stream-json")
	rootCmd.PersistentFlags().StringVar(&inputFormat, "input-format", "text", "headless input format: text or stream-json")
	rootCmd.PersistentFlags().IntVar(&headlessTurns, "max-turns", 0, "maximum model/tool rounds in headless mode (0 means no turn cap)")
	rootCmd.PersistentFlags().DurationVar(&headlessLimit, "timeout", 0, "overall headless execution deadline, for example 30m (0 disables)")
	rootCmd.PersistentFlags().Float64Var(&headlessBudget, "max-budget-usd", 0, "maximum estimated provider cost in headless mode (0 disables)")
	rootCmd.PersistentFlags().StringVar(&permissionMode, "permission-mode", "", "permission mode for this run: default, acceptEdits, dontAsk, bypassPermissions, or plan")
	rootCmd.PersistentFlags().BoolVar(&skipPermissions, "dangerously-skip-permissions", false, "bypass permission prompts for this run (sandbox, path boundaries, and hard safety checks remain active)")
	rootCmd.PersistentFlags().StringSliceVar(&allowedToolsCompat, "allowedTools", nil, "pre-approve matching tools for this run without widening --tools (Claude-compatible)")
	rootCmd.PersistentFlags().StringSliceVar(&allowedTools, "allowed-tools", nil, "pre-approve matching tools for this run without widening --tools")
	rootCmd.PersistentFlags().StringSliceVar(&toolCeiling, "tools", nil, "restrict available tools to these exact names (comma-separated; empty means no tools)")
	rootCmd.PersistentFlags().StringSliceVar(&deniedTools, "disallowed-tools", nil, "deny matching tools/calls; bare names are also hidden from the model")
	rootCmd.PersistentFlags().StringSliceVar(&deniedToolsCompat, "disallowedTools", nil, "deny matching tools/calls; bare names are also hidden from the model (Claude-compatible)")
	rootCmd.PersistentFlags().StringArrayVar(&addDirs, "add-dir", nil, "grant access to a directory outside the workspace (repeatable; in-memory for this run)")
	rootCmd.PersistentFlags().StringVar(&systemPrompt, "system-prompt", "", "replace the generated system prompt for this run")
	rootCmd.PersistentFlags().StringVar(&systemPromptFile, "system-prompt-file", "", "replace the generated system prompt from a UTF-8 file for this run")
	rootCmd.PersistentFlags().StringVar(&appendSystemPrompt, "append-system-prompt", "", "append instructions to the system prompt for this run")
	rootCmd.PersistentFlags().StringVar(&appendSystemFile, "append-system-prompt-file", "", "append instructions from a UTF-8 file for this run")
	rootCmd.PersistentFlags().StringVar(&jsonSchema, "json-schema", "", "validate the final headless result against an inline JSON Schema")
	rootCmd.PersistentFlags().BoolVar(&bare, "bare", false, "start a minimal Read/Edit/Bash runtime without auto-discovery (Claude-compatible)")
	rootCmd.PersistentFlags().StringVar(&debugCategories, "debug", "", "enable file debug logging with optional categories, for example api,mcp")
	rootCmd.PersistentFlags().Lookup("debug").NoOptDefVal = "*"
	rootCmd.PersistentFlags().StringVar(&debugFile, "debug-file", "", "write debug logs to this file (implicitly enables --debug)")

	// Version command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version number",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("gokin version %s\n", version)
		},
	})

	// Update command
	rootCmd.AddCommand(newUpdateCmd())
	rootCmd.AddCommand(newEvalCmd())
	rootCmd.AddCommand(newDoctorCmd())
	rootCmd.AddCommand(newBackgroundAgentsCmd())
	rootCmd.AddCommand(newBackgroundLogsCmd())
	rootCmd.AddCommand(newBackgroundStopCmd())
	rootCmd.AddCommand(newBackgroundRespawnCmd())
	rootCmd.AddCommand(newBackgroundSendCmd())
	rootCmd.AddCommand(newBackgroundAttachCmd())

	rootInvocationArgs = normalizeOptionalDebugArgs(os.Args[1:])
	rootCmd.SetArgs(rootInvocationArgs)
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runApp(cmd *cobra.Command, args []string) (runErr error) {
	backgroundWorker, err := beginBackgroundWorker()
	if err != nil {
		return err
	}
	if backgroundWorker != nil {
		defer func() { backgroundWorker.finish(runErr) }()
		stopSignals := backgroundWorker.installSignalContext(cmd)
		defer stopSignals()
	}
	if backgroundMode {
		if backgroundWorker != nil {
			return fmt.Errorf("background worker invocation retained --background")
		}
		return launchBackgroundSession(cmd, args)
	}

	effectiveHeadless := headless || printMode
	headlessFormat, err := resolveHeadlessOutputFormat(effectiveHeadless, outputFormat)
	if err != nil {
		return err
	}
	// Once a structured headless mode is recognized, every startup/runtime failure must
	// still produce exactly one result envelope. RunHeadlessWithOptions writes
	// its own terminal envelope; this defer covers failures before execution
	// begins (CLI validation, config/auth, app init, and exact resume).
	jsonEnvelopeWritten := false
	failureKind := "cli"
	failureSessionID := strings.TrimSpace(resumeSession)
	if strings.TrimSpace(sessionID) != "" {
		failureSessionID = strings.TrimSpace(sessionID)
	}
	defer func() {
		if runErr == nil || !effectiveHeadless || !isStructuredHeadlessOutput(headlessFormat) || jsonEnvelopeWritten {
			return
		}
		if encodeErr := writeHeadlessFailure(os.Stdout, failureSessionID, failureKind, runErr); encodeErr != nil {
			runErr = errors.Join(runErr, encodeErr)
		}
	}()

	failureKind = "validation"
	headlessInputFormat, err := resolveHeadlessInputFormat(effectiveHeadless, inputFormat, headlessFormat)
	if err != nil {
		return err
	}
	if err := validateHeadlessExecutionLimits(effectiveHeadless, headlessTurns, headlessLimit, headlessBudget); err != nil {
		return err
	}
	resolvedPermissionMode, err := resolveCLIPermissionMode(permissionMode, skipPermissions)
	if err != nil {
		return err
	}
	flagChanged := func(name string, nonEmptyFallback bool) bool {
		if cmd == nil {
			return nonEmptyFallback
		}
		return cmd.Flags().Changed(name)
	}
	resolvedDebug, err := resolveCLIDebug(cliDebugFlags{
		debug:        debugCategories,
		debugSet:     flagChanged("debug", debugCategories != ""),
		file:         debugFile,
		debugFileSet: flagChanged("debug-file", debugFile != ""),
	})
	if err != nil {
		return err
	}
	resolvedJSONSchema, err := resolveCLIJSONSchema(
		effectiveHeadless,
		headlessFormat,
		jsonSchema,
		flagChanged("json-schema", jsonSchema != ""),
	)
	if err != nil {
		return err
	}
	resolvedSystemPrompt, err := resolveCLISystemPrompt(cliSystemPromptFlags{
		replacement:     systemPrompt,
		replacementSet:  flagChanged("system-prompt", systemPrompt != ""),
		replacementFile: systemPromptFile,
		fileSet:         flagChanged("system-prompt-file", systemPromptFile != ""),
		append:          appendSystemPrompt,
		appendSet:       flagChanged("append-system-prompt", appendSystemPrompt != ""),
		appendFile:      appendSystemFile,
		appendFileSet:   flagChanged("append-system-prompt-file", appendSystemFile != ""),
	})
	if err != nil {
		return err
	}
	resolvedAllowedToolRules, err := resolveCLIAllowedToolRules(append(
		append([]string(nil), allowedTools...),
		allowedToolsCompat...,
	))
	if err != nil {
		return err
	}
	resolvedDeniedToolRules, err := resolveCLIDeniedToolRules(append(
		append([]string(nil), deniedTools...),
		deniedToolsCompat...,
	))
	if err != nil {
		return err
	}
	resumeID, err := validateResumeSelection(continueLast, resumeSession)
	if err != nil {
		if continueLast && resumeSession != "" {
			failureKind = "resume_conflict"
		} else {
			failureKind = "session_invalid_id"
		}
		return err
	}
	requestedSessionID, err := validateNewSessionSelection(sessionID, continueLast, resumeID, forkSession)
	if err != nil {
		failureKind = "session_invalid_selection"
		return err
	}
	if err := validateSessionPersistenceFlags(noSessionPersistence, continueLast, resumeID, forkSession); err != nil {
		return err
	}
	restoreBareEnv, err := configureBareEnvironment(bare)
	if err != nil {
		return err
	}
	defer restoreBareEnv()
	if resolvedDebug.enabled {
		if err := logging.EnablePathLogging(
			resolvedDebug.path, resolvedDebug.level, resolvedDebug.filter); err != nil {
			return fmt.Errorf("enable debug logging: %w", err)
		}
		defer logging.Close()
		logging.Debug("debug logging enabled",
			"category", "startup",
			"path", logging.CurrentLogPath(),
			"filter", resolvedDebug.filter,
			"level", resolvedDebug.level)
		defer func() {
			if runErr != nil {
				logging.Error("gokin run failed",
					"category", "lifecycle",
					"failure_kind", failureKind,
					"session_id", failureSessionID,
					"error", runErr)
				return
			}
			logging.Info("gokin run completed",
				"category", "lifecycle",
				"session_id", failureSessionID)
		}()
	}

	resolvedPrompt := prompt
	if effectiveHeadless {
		failureKind = "input"
		if headlessInputFormat == headlessInputText {
			pipedInput, inputErr := readHeadlessStdin(cmd)
			if inputErr != nil {
				return inputErr
			}
			resolvedPrompt, inputErr = resolveHeadlessPrompt(prompt, args, pipedInput)
			if inputErr != nil {
				return inputErr
			}
		} else {
			if strings.TrimSpace(prompt) != "" || strings.TrimSpace(strings.Join(args, " ")) != "" {
				return fmt.Errorf("--input-format stream-json reads prompts from stdin; do not also pass --prompt or positional input")
			}
			if stdinIsTerminal(cmd) {
				return fmt.Errorf("--input-format stream-json requires redirected stdin")
			}
		}
	} else {
		resolvedPrompt, err = resolveInteractivePrompt(prompt, args)
		if err != nil {
			return err
		}
	}

	// Bind the process to an explicit --config BEFORE anything can write a
	// config file. The wizard below resolves the config location itself, so a
	// late binding would send the API key to the default location while the run
	// keeps reading the file the operator named.
	if strings.TrimSpace(cfgFile) != "" {
		config.SetExplicitConfigPath(cfgFile)
	}

	// Run setup wizard if requested
	if runSetup {
		failureKind = "setup"
		if effectiveHeadless {
			// The auto-invoked path below (triggered by ErrMissingAuth) has
			// always refused to run the wizard in headless mode; the
			// explicit --setup flag lacked the same guard, so
			// `--headless --setup` either blocked forever on stdin (a live
			// TTY) or died with a confusing "EOF" (redirected/closed stdin)
			// instead of headless mode's documented "never block, fail
			// clearly" contract.
			return fmt.Errorf("--setup requires an interactive terminal; it cannot run with --headless")
		}
		if err := setup.RunSetupWizard(); err != nil {
			return err
		}
		// Continue to start the app after setup
	}

	// Load configuration
	failureKind = "configuration"
	cfg, err := loadConfiguredConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	if err := applyRunConfigOverrides(
		cfg, version, provider, model, baseURL, noSessionPersistence); err != nil {
		return err
	}
	applyBareRunConfig(cfg, bare)
	applyDebugRunConfig(cfg, resolvedDebug)
	logging.Debug("runtime configuration resolved",
		"category", "startup",
		"provider", cfg.API.ActiveProvider,
		"model", cfg.Model.Name,
		"headless", effectiveHeadless,
		"bare", cfg.Bare)
	if err := applyCLIPermissionMode(cfg, resolvedPermissionMode); err != nil {
		return err
	}

	// Validate configuration - if no API key, run setup wizard automatically
	if err := cfg.Validate(); err != nil {
		if errors.Is(err, config.ErrMissingAuth) {
			if effectiveHeadless {
				return err
			}
			// No API key configured - run setup wizard
			if err := setup.RunSetupWizard(); err != nil {
				return err
			}
			// Reload config after setup
			cfg, err = loadConfiguredConfig(cfgFile)
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			if err := applyRunConfigOverrides(
				cfg, version, provider, model, baseURL, noSessionPersistence); err != nil {
				return err
			}
			applyBareRunConfig(cfg, bare)
			applyDebugRunConfig(cfg, resolvedDebug)
			if err := applyCLIPermissionMode(cfg, resolvedPermissionMode); err != nil {
				return err
			}
			// Re-validate
			if err := cfg.Validate(); err != nil {
				return err
			}
		} else {
			return err
		}
	}

	// Get working directory
	failureKind = "app_init"
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// --add-dir grants (repeatable): resolve, refuse ungrantable locations, and
	// append in-memory only (never persisted).
	// Works for interactive, headless, and eval launches; the builder propagates
	// AllowedDirs to every path-scoping tool and seeds the agent runner at boot.
	if err := applyAddDirFlags(cfg, addDirs); err != nil {
		return err
	}

	// Create the application
	startupToolDenies := optionalRuntimeDeniesForCLIRules(resolvedDeniedToolRules)
	application, err := app.NewWithOptions(cfg, workDir, app.BuildOptions{
		NonInteractive:               effectiveHeadless,
		Bare:                         bare,
		StartupToolCapabilityAllowed: toolCeiling,
		StartupToolCapabilityDenied:  startupToolDenies,
	})
	if err != nil {
		return fmt.Errorf("failed to create application: %w", err)
	}
	logging.Debug("application initialized",
		"category", "startup",
		"work_dir", workDir,
		"tools", application.GetToolRegistry().Names())
	if err := application.ConfigureRunSystemPrompt(
		resolvedSystemPrompt.replacement, resolvedSystemPrompt.append); err != nil {
		return err
	}
	failureKind = "tool_policy"
	if err := application.ConfigureRunPermissionRules(
		resolvedAllowedToolRules, resolvedDeniedToolRules); err != nil {
		return err
	}
	capabilityDenies, err := capabilityDeniesForCLIRules(
		application.GetToolRegistry().Names(), resolvedDeniedToolRules)
	if err != nil {
		return err
	}
	if err := application.ConfigureToolCapability(toolCeiling, capabilityDenies); err != nil {
		return err
	}

	failureKind = "resume_failed"
	sessionLease, selectedSessionID, err := prepareSessionForRun(
		application, cfg.Session.Enabled, resumeID, continueLast, requestedSessionID, forkSession)
	if selectedSessionID != "" {
		failureSessionID = selectedSessionID
	}
	logging.Debug("session prepared",
		"category", "session",
		"session_id", selectedSessionID,
		"resume", resumeID != "" || continueLast,
		"fork", forkSession,
		"persistent", cfg.Session.Enabled)
	if err != nil {
		failureKind = sessionPreparationErrorKind(err, resumeID != "" || continueLast)
		return err
	}
	if backgroundWorker != nil {
		if err := backgroundWorker.setSessionID(selectedSessionID); err != nil {
			if sessionLease != nil {
				_ = sessionLease.Release()
			}
			failureKind = "background_persistence"
			return fmt.Errorf("publish background session identity: %w", err)
		}
	}
	if sessionLease != nil {
		if err := application.AdoptSessionWriterLease(sessionLease); err != nil {
			_ = sessionLease.Release()
			failureKind = "session_lease"
			return fmt.Errorf("adopt session writer lease: %w", err)
		}
		defer func() {
			if releaseErr := application.ReleaseSessionWriterLease(); releaseErr != nil {
				// Descriptor-owned OS locks are released by process exit even if
				// the explicit unlock reports an error; keep the already-emitted
				// JSON status aligned with the process exit code.
				fmt.Fprintf(os.Stderr, "Warning: failed to release session writer lease: %v\n", releaseErr)
			}
		}()
	}

	if effectiveHeadless {
		failureKind = "execution"
		jsonEnvelopeWritten = isStructuredHeadlessOutput(headlessFormat)
		opts := app.HeadlessOptions{
			OutputFormat: headlessFormat,
			Stdout:       os.Stdout,
			Stderr:       os.Stderr,
			JSONSchema:   resolvedJSONSchema,
			MaxTurns:     headlessTurns,
			Timeout:      headlessLimit,
			MaxBudgetUSD: headlessBudget,
		}
		if headlessInputFormat == headlessInputStreamJSON {
			failureKind = "input"
			return runHeadlessInputStream(
				cmd.Context(), application, cmd.InOrStdin(), opts, application.GetSession().GetID())
		}
		failureKind = "execution"
		if backgroundWorker != nil {
			return runBackgroundHeadlessLoop(
				cmd.Context(), application, resolvedPrompt, opts, backgroundWorker)
		}
		_, err := application.RunHeadlessWithOptions(cmd.Context(), resolvedPrompt, app.HeadlessOptions{
			OutputFormat: opts.OutputFormat,
			Stdout:       opts.Stdout,
			Stderr:       opts.Stderr,
			JSONSchema:   opts.JSONSchema,
			MaxTurns:     opts.MaxTurns,
			Timeout:      opts.Timeout,
			MaxBudgetUSD: opts.MaxBudgetUSD,
		})
		return err
	}

	// Interactive resume is also fail-closed (prepared above): an explicitly
	// requested context must never silently turn into a fresh conversation.
	switch {
	case forkSession:
		fmt.Printf("Forked resumed context into session %s.\n", selectedSessionID)
	case resumeID != "":
		fmt.Printf("Resumed session %s.\n", resumeID)
	case continueLast:
		fmt.Println("Resumed previous session.")
	}

	// Check for updates on startup (non-blocking notification). Wrapped in
	// defer-recover so a panic inside the update path (network library bug,
	// nil-deref in update.NewUpdater, etc.) doesn't crash gokin before the
	// TUI even starts. v0.78.1 wrapped the equivalent app/ goroutines —
	// this is the cmd/ counterpart.
	if !bare {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logging.Error("update-check goroutine panicked", "panic", r)
				}
			}()
			CheckForUpdateOnStartup(cfg, application)
		}()
	}

	fmt.Println("\nStarting Gokin...")
	return application.RunWithInitialPrompt(resolvedPrompt)
}

func resolveCLIJSONSchema(
	headless bool,
	format app.HeadlessOutputFormat,
	raw string,
	set bool,
) (*app.StructuredOutputSchema, error) {
	if !set {
		return nil, nil
	}
	if !headless {
		return nil, fmt.Errorf("--json-schema requires --headless or --print")
	}
	if format != app.HeadlessOutputJSON && format != app.HeadlessOutputStreamJSON {
		return nil, fmt.Errorf(
			"--json-schema requires --output-format json or stream-json")
	}
	schema, err := app.CompileStructuredOutputSchema(raw)
	if err != nil {
		return nil, err
	}
	return schema, nil
}

type cliSystemPromptFlags struct {
	replacement     string
	replacementSet  bool
	replacementFile string
	fileSet         bool
	append          string
	appendSet       bool
	appendFile      string
	appendFileSet   bool
}

type cliSystemPromptOptions struct {
	replacement *string
	append      string
}

func resolveCLISystemPrompt(flags cliSystemPromptFlags) (cliSystemPromptOptions, error) {
	if flags.replacementSet && flags.fileSet {
		return cliSystemPromptOptions{}, fmt.Errorf(
			"--system-prompt conflicts with --system-prompt-file")
	}
	if flags.appendSet && flags.appendFileSet {
		return cliSystemPromptOptions{}, fmt.Errorf(
			"--append-system-prompt conflicts with --append-system-prompt-file")
	}

	var resolved cliSystemPromptOptions
	switch {
	case flags.replacementSet:
		value := flags.replacement
		resolved.replacement = &value
	case flags.fileSet:
		value, err := readBoundedSystemPromptFile(
			flags.replacementFile, "--system-prompt-file")
		if err != nil {
			return cliSystemPromptOptions{}, err
		}
		resolved.replacement = &value
	}
	switch {
	case flags.appendSet:
		resolved.append = flags.append
	case flags.appendFileSet:
		value, err := readBoundedSystemPromptFile(
			flags.appendFile, "--append-system-prompt-file")
		if err != nil {
			return cliSystemPromptOptions{}, err
		}
		resolved.append = value
	}

	if resolved.replacement != nil {
		if err := validateCLISystemPromptText(
			"--system-prompt", *resolved.replacement); err != nil {
			return cliSystemPromptOptions{}, err
		}
	}
	if err := validateCLISystemPromptText(
		"--append-system-prompt", resolved.append); err != nil {
		return cliSystemPromptOptions{}, err
	}
	replacementBytes := 0
	if resolved.replacement != nil {
		replacementBytes = len(*resolved.replacement)
	}
	if replacementBytes+len(resolved.append) > app.MaxRunSystemPromptBytes {
		return cliSystemPromptOptions{}, fmt.Errorf(
			"combined run system prompt exceeds %d KiB limit",
			app.MaxRunSystemPromptBytes>>10)
	}
	return resolved, nil
}

func readBoundedSystemPromptFile(path, flagName string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s requires a file path", flagName)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s %q: %w", flagName, path, err)
	}
	defer file.Close()

	content, err := io.ReadAll(io.LimitReader(file, app.MaxRunSystemPromptBytes+1))
	if err != nil {
		return "", fmt.Errorf("read %s %q: %w", flagName, path, err)
	}
	if len(content) > app.MaxRunSystemPromptBytes {
		return "", fmt.Errorf(
			"%s file exceeds %d KiB limit",
			flagName, app.MaxRunSystemPromptBytes>>10)
	}
	value := string(content)
	if err := validateCLISystemPromptText(flagName, value); err != nil {
		return "", err
	}
	return value, nil
}

func validateCLISystemPromptText(label, value string) error {
	if len(value) > app.MaxRunSystemPromptBytes {
		return fmt.Errorf("%s exceeds %d KiB limit", label, app.MaxRunSystemPromptBytes>>10)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must contain valid UTF-8", label)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s must not contain a NUL byte", label)
	}
	return nil
}

func resolveHeadlessOutputFormat(headless bool, raw string) (app.HeadlessOutputFormat, error) {
	format := app.HeadlessOutputFormat(strings.ToLower(strings.TrimSpace(raw)))
	if format == "" {
		format = app.HeadlessOutputText
	}
	if format != app.HeadlessOutputText && format != app.HeadlessOutputJSON && format != app.HeadlessOutputStreamJSON {
		return "", fmt.Errorf("invalid --output-format %q (want text, json, or stream-json)", raw)
	}
	if !headless && format != app.HeadlessOutputText {
		return "", fmt.Errorf("--output-format %s requires --headless", format)
	}
	return format, nil
}

type headlessInputMode string

const (
	headlessInputText       headlessInputMode = "text"
	headlessInputStreamJSON headlessInputMode = "stream-json"
)

func resolveHeadlessInputFormat(
	headless bool,
	raw string,
	output app.HeadlessOutputFormat,
) (headlessInputMode, error) {
	format := headlessInputMode(strings.ToLower(strings.TrimSpace(raw)))
	if format == "" {
		format = headlessInputText
	}
	if format != headlessInputText && format != headlessInputStreamJSON {
		return "", fmt.Errorf("invalid --input-format %q (want text or stream-json)", raw)
	}
	if !headless && format != headlessInputText {
		return "", fmt.Errorf("--input-format %s requires --headless or --print", format)
	}
	if format == headlessInputStreamJSON && output != app.HeadlessOutputStreamJSON {
		return "", fmt.Errorf("--input-format stream-json requires --output-format stream-json")
	}
	return format, nil
}

func isStructuredHeadlessOutput(format app.HeadlessOutputFormat) bool {
	return format == app.HeadlessOutputJSON || format == app.HeadlessOutputStreamJSON
}

func validateHeadlessExecutionLimits(headless bool, maxTurns int, timeout time.Duration, maxBudgetUSD float64) error {
	if maxTurns < 0 {
		return fmt.Errorf("--max-turns must be zero or greater")
	}
	if timeout < 0 {
		return fmt.Errorf("--timeout must be zero or greater")
	}
	if maxBudgetUSD < 0 {
		return fmt.Errorf("--max-budget-usd must be zero or greater")
	}
	if !headless && maxTurns != 0 {
		return fmt.Errorf("--max-turns requires --headless or --print")
	}
	if !headless && timeout != 0 {
		return fmt.Errorf("--timeout requires --headless or --print")
	}
	if !headless && maxBudgetUSD != 0 {
		return fmt.Errorf("--max-budget-usd requires --headless or --print")
	}
	return nil
}

type cliPermissionMode string

const (
	cliPermissionInherit     cliPermissionMode = ""
	cliPermissionDefault     cliPermissionMode = "default"
	cliPermissionAcceptEdits cliPermissionMode = "acceptEdits"
	cliPermissionDontAsk     cliPermissionMode = "dontAsk"
	cliPermissionBypass      cliPermissionMode = "bypassPermissions"
	cliPermissionPlan        cliPermissionMode = "plan"
)

// resolveCLIPermissionMode accepts Claude-Code-compatible spellings while
// keeping one canonical internal value. The dangerous alias is explicit
// authority to bypass prompts; combining it with a contradictory mode is
// rejected instead of silently choosing the more permissive interpretation.
func resolveCLIPermissionMode(raw string, dangerouslySkip bool) (cliPermissionMode, error) {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	normalized = strings.NewReplacer("-", "", "_", "", " ", "").Replace(normalized)

	mode := cliPermissionInherit
	switch normalized {
	case "":
	case "default":
		mode = cliPermissionDefault
	case "acceptedits":
		mode = cliPermissionAcceptEdits
	case "dontask":
		mode = cliPermissionDontAsk
	case "bypasspermissions":
		mode = cliPermissionBypass
	case "plan":
		mode = cliPermissionPlan
	default:
		return "", fmt.Errorf(
			"invalid --permission-mode %q (want default, acceptEdits, dontAsk, bypassPermissions, or plan)",
			raw,
		)
	}
	if dangerouslySkip {
		if mode != cliPermissionInherit &&
			mode != cliPermissionDefault &&
			mode != cliPermissionBypass {
			return "", fmt.Errorf(
				"--dangerously-skip-permissions conflicts with --permission-mode %s",
				mode,
			)
		}
		mode = cliPermissionBypass
	}
	return mode, nil
}

var acceptEditsPermissionTools = []string{
	"write",
	"edit",
	"batch",
	"refactor",
	"copy",
	"move",
	"mkdir",
	"delete",
}

// applyCLIPermissionMode mutates only the in-memory run config. In particular,
// bypassing permission prompts does not disable the bash sandbox, directory
// boundaries, command safety validator, hooks, or invocation tool ceilings.
func applyCLIPermissionMode(cfg *config.Config, mode cliPermissionMode) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	switch mode {
	case cliPermissionInherit:
		return nil
	case cliPermissionDefault:
		cfg.Permission.Enabled = true
		cfg.Permission.DontAsk = false
		cfg.Plan.Enabled = false
		return nil
	case cliPermissionAcceptEdits:
		cfg.Permission.Enabled = true
		cfg.Permission.DontAsk = false
		cfg.Plan.Enabled = false
		if cfg.Permission.Rules == nil {
			cfg.Permission.Rules = make(map[string]string)
		}
		for _, tool := range acceptEditsPermissionTools {
			cfg.Permission.Rules[tool] = "allow"
		}
		return nil
	case cliPermissionDontAsk:
		cfg.Permission.Enabled = true
		cfg.Permission.DontAsk = true
		cfg.Plan.Enabled = false
		return nil
	case cliPermissionBypass:
		cfg.Permission.Enabled = false
		cfg.Permission.DontAsk = false
		cfg.Plan.Enabled = false
		return nil
	case cliPermissionPlan:
		cfg.Permission.Enabled = true
		cfg.Permission.DontAsk = false
		cfg.Plan.Enabled = true
		// Exiting plan mode remains an explicit approval boundary. In headless
		// mode no interactive approver exists, so mutations stay fail-closed.
		cfg.Plan.RequireApproval = true
		return nil
	default:
		return fmt.Errorf("unsupported permission mode %q", mode)
	}
}

func resolveCLIAllowedToolRules(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	rules, err := permission.ParseTemporaryToolGrantList(strings.Join(values, " "))
	if err != nil {
		return nil, fmt.Errorf("invalid --allowedTools/--allowed-tools: %w", err)
	}
	return rules, nil
}

func resolveCLIDeniedToolRules(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	rules, err := permission.ParseTemporaryToolDenyList(strings.Join(values, " "))
	if err != nil {
		return nil, fmt.Errorf("invalid --disallowedTools/--disallowed-tools: %w", err)
	}
	return rules, nil
}

// capabilityDeniesForCLIRules expands bare/wildcard deny rules against the
// current registry so matching tools disappear from the model schema. Scoped
// Bash rules remain visible and are enforced by the permission manager.
// Runtime deny matching also remains installed for hallucinated/late tools.
func capabilityDeniesForCLIRules(available, rules []string) ([]string, error) {
	if len(rules) == 0 {
		return nil, nil
	}
	availableSet := make(map[string]bool, len(available))
	for _, name := range available {
		availableSet[name] = true
	}
	deniedSet := make(map[string]bool)
	for _, rule := range rules {
		if strings.ContainsRune(rule, '(') {
			continue
		}
		matched := false
		for _, name := range available {
			if permission.ToolDenyRuleMatchesName(rule, name) {
				deniedSet[name] = true
				matched = true
			}
		}
		// Optional runtimes may be absent because this invocation's own deny
		// policy let Builder skip their startup. Retain that deny in the final
		// executor ceiling instead of rejecting it as an unknown tool.
		for _, name := range []string{"repl_exec", "harness"} {
			if !availableSet[name] && permission.ToolDenyRuleMatchesName(rule, name) {
				matched = true
			}
		}
		if !strings.ContainsRune(rule, '*') && !matched && !availableSet[rule] {
			return nil, fmt.Errorf(
				"unknown tool in --disallowedTools/--disallowed-tools: %s", rule)
		}
	}
	denied := make([]string, 0, len(deniedSet))
	for name := range deniedSet {
		denied = append(denied, name)
	}
	sort.Strings(denied)
	if len(denied) == 0 {
		return nil, nil
	}
	return denied, nil
}

func optionalRuntimeDeniesForCLIRules(rules []string) []string {
	denied := make([]string, 0, 2)
	for _, name := range []string{"repl_exec", "harness"} {
		for _, rule := range rules {
			if !strings.ContainsRune(rule, '(') && permission.ToolDenyRuleMatchesName(rule, name) {
				denied = append(denied, name)
				break
			}
		}
	}
	if len(denied) == 0 {
		return nil
	}
	return denied
}

type headlessInputRunner interface {
	RunHeadlessWithOptions(context.Context, string, app.HeadlessOptions) (app.HeadlessResult, error)
}

type headlessStreamInputRecord struct {
	SchemaVersion int             `json:"schema_version,omitempty"`
	Type          string          `json:"type"`
	Prompt        string          `json:"prompt,omitempty"`
	Message       json.RawMessage `json:"message,omitempty"`
}

type headlessStreamInputMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type headlessStreamInputTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// runHeadlessInputStream consumes one user record per JSONL line and emits one
// complete stream-json turn for each record. It intentionally reads the next
// line only after the current turn has completed, providing natural backpressure
// and keeping one App/session strictly single-writer.
func runHeadlessInputStream(
	ctx context.Context,
	runner headlessInputRunner,
	reader io.Reader,
	opts app.HeadlessOptions,
	sessionID string,
) error {
	if runner == nil {
		failure := fmt.Errorf("headless application is not initialized")
		_ = writeHeadlessFailure(opts.Stdout, sessionID, "app_init", failure)
		return failure
	}
	if reader == nil {
		failure := fmt.Errorf("--input-format stream-json requires redirected stdin")
		_ = writeHeadlessFailure(opts.Stdout, sessionID, "input", failure)
		return failure
	}
	if opts.StreamState == nil {
		opts.StreamState = app.NewHeadlessStreamState()
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), maxHeadlessStdinBytes+1)
	records := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) > maxHeadlessStdinBytes {
			failure := fmt.Errorf(
				"stream-json input record %d exceeds %d MiB limit",
				records+1, maxHeadlessStdinBytes>>20)
			if writeErr := writeHeadlessFailure(opts.Stdout, sessionID, "input", failure); writeErr != nil {
				return errors.Join(failure, writeErr)
			}
			return failure
		}
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			// Every other exit from this loop emits a terminal JSON record;
			// cancellation between records must not be the one shape that
			// leaves a machine consumer with a non-zero exit and no envelope.
			if writeErr := writeHeadlessFailure(opts.Stdout, sessionID, "cancelled", err); writeErr != nil {
				return errors.Join(err, writeErr)
			}
			return err
		}
		prompt, err := parseHeadlessStreamInput(line)
		if err != nil {
			failure := fmt.Errorf("invalid stream-json input record %d: %w", records+1, err)
			if writeErr := writeHeadlessFailure(opts.Stdout, sessionID, "input", failure); writeErr != nil {
				return errors.Join(failure, writeErr)
			}
			return failure
		}
		records++
		if _, err := runner.RunHeadlessWithOptions(ctx, prompt, opts); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		failure := fmt.Errorf(
			"read stream-json input (each record must be at most %d MiB): %w",
			maxHeadlessStdinBytes>>20, err)
		if writeErr := writeHeadlessFailure(opts.Stdout, sessionID, "input", failure); writeErr != nil {
			return errors.Join(failure, writeErr)
		}
		return failure
	}
	if records == 0 {
		failure := fmt.Errorf("--input-format stream-json received no user records")
		if writeErr := writeHeadlessFailure(opts.Stdout, sessionID, "input", failure); writeErr != nil {
			return errors.Join(failure, writeErr)
		}
		return failure
	}
	return nil
}

func parseHeadlessStreamInput(data []byte) (string, error) {
	var record headlessStreamInputRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return "", fmt.Errorf("decode JSON: %w", err)
	}
	if record.SchemaVersion != 0 && record.SchemaVersion != app.HeadlessSchemaVersion {
		return "", fmt.Errorf(
			"unsupported schema_version %d (want %d)",
			record.SchemaVersion, app.HeadlessSchemaVersion)
	}
	record.Type = strings.ToLower(strings.TrimSpace(record.Type))
	if record.Type != "user" {
		return "", fmt.Errorf("type must be %q", "user")
	}

	direct := strings.TrimSpace(record.Prompt)
	hasMessage := len(record.Message) > 0 && string(record.Message) != "null"
	if direct != "" && hasMessage {
		return "", fmt.Errorf("use either prompt or message, not both")
	}
	if direct != "" {
		return direct, nil
	}
	if !hasMessage {
		return "", fmt.Errorf("user record requires prompt or message")
	}

	var message headlessStreamInputMessage
	if err := json.Unmarshal(record.Message, &message); err != nil {
		return "", fmt.Errorf("decode message: %w", err)
	}
	if strings.ToLower(strings.TrimSpace(message.Role)) != "user" {
		return "", fmt.Errorf("message.role must be %q", "user")
	}
	if len(message.Content) == 0 || string(message.Content) == "null" {
		return "", fmt.Errorf("message.content is required")
	}

	var content string
	if err := json.Unmarshal(message.Content, &content); err == nil {
		content = strings.TrimSpace(content)
		if content == "" {
			return "", fmt.Errorf("message.content must not be empty")
		}
		return content, nil
	}

	var blocks []headlessStreamInputTextBlock
	if err := json.Unmarshal(message.Content, &blocks); err != nil {
		return "", fmt.Errorf("message.content must be a string or an array of text blocks")
	}
	texts := make([]string, 0, len(blocks))
	for i, block := range blocks {
		if strings.ToLower(strings.TrimSpace(block.Type)) != "text" {
			return "", fmt.Errorf("message.content[%d].type must be %q", i, "text")
		}
		if text := strings.TrimSpace(block.Text); text != "" {
			texts = append(texts, text)
		}
	}
	content = strings.Join(texts, "\n")
	if content == "" {
		return "", fmt.Errorf("message.content must contain non-empty text")
	}
	return content, nil
}

// readHeadlessStdin reads redirected input but never touches a live terminal.
// That distinction keeps `gokin --headless --prompt ...` non-blocking while
// still supporting `cat build.log | gokin -p "diagnose this"`.
func readHeadlessStdin(cmd *cobra.Command) (string, error) {
	if cmd == nil {
		return "", nil
	}
	reader := cmd.InOrStdin()
	if reader == nil {
		return "", nil
	}
	if file, ok := reader.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		return "", nil
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxHeadlessStdinBytes+1))
	if err != nil {
		return "", fmt.Errorf("read headless stdin: %w", err)
	}
	if len(data) > maxHeadlessStdinBytes {
		return "", fmt.Errorf("headless stdin exceeds %d MiB limit", maxHeadlessStdinBytes>>20)
	}
	return string(data), nil
}

func stdinIsTerminal(cmd *cobra.Command) bool {
	if cmd == nil {
		return false
	}
	file, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

// resolveHeadlessPrompt accepts the same automation shapes users expect from
// modern coding CLIs: a flag, one positional query, stdin, or a query plus
// piped context. Flag and positional query are mutually exclusive so a shell
// typo cannot silently change the instruction.
func resolveHeadlessPrompt(flagPrompt string, args []string, pipedInput string) (string, error) {
	flagPrompt = strings.TrimSpace(flagPrompt)
	positionalPrompt := strings.TrimSpace(strings.Join(args, " "))
	pipedInput = strings.TrimSpace(pipedInput)

	if flagPrompt != "" && positionalPrompt != "" {
		return "", fmt.Errorf("headless prompt is ambiguous: use either --prompt or a positional prompt, not both")
	}
	query := flagPrompt
	if query == "" {
		query = positionalPrompt
	}
	switch {
	case query != "" && pipedInput != "":
		return query + "\n\n" + pipedInput, nil
	case query != "":
		return query, nil
	case pipedInput != "":
		return pipedInput, nil
	default:
		return "", fmt.Errorf("a prompt is required in headless mode; pass --prompt, a positional prompt, or stdin")
	}
}

func resolveInteractivePrompt(flagPrompt string, args []string) (string, error) {
	flagPrompt = strings.TrimSpace(flagPrompt)
	positionalPrompt := strings.TrimSpace(strings.Join(args, " "))
	if flagPrompt != "" && positionalPrompt != "" {
		return "", fmt.Errorf("initial prompt is ambiguous: use either --prompt or a positional prompt, not both")
	}
	if flagPrompt != "" {
		return flagPrompt, nil
	}
	return positionalPrompt, nil
}

func validateResumeSelection(continueLast bool, rawID string) (string, error) {
	if continueLast && rawID != "" {
		return "", fmt.Errorf("--continue and --resume are mutually exclusive")
	}
	if rawID == "" {
		return "", nil
	}
	if strings.TrimSpace(rawID) != rawID {
		return "", fmt.Errorf("invalid --resume session ID: leading or trailing whitespace is not allowed")
	}
	if err := chat.ValidateSessionID(rawID); err != nil {
		return "", fmt.Errorf("invalid --resume session ID: %w", err)
	}
	return rawID, nil
}

func validateNewSessionSelection(rawID string, continueLast bool, resumeID string, fork bool) (string, error) {
	if fork && !continueLast && resumeID == "" {
		return "", fmt.Errorf("--fork-session requires --continue or --resume")
	}
	if rawID != "" && (continueLast || resumeID != "" || fork) {
		return "", fmt.Errorf("--session-id cannot be combined with --continue, --resume, or --fork-session")
	}
	if rawID == "" {
		return "", nil
	}
	if strings.TrimSpace(rawID) != rawID {
		return "", fmt.Errorf("invalid --session-id: leading or trailing whitespace is not allowed")
	}
	parsed, err := uuid.Parse(rawID)
	if err != nil || parsed.String() != rawID {
		return "", fmt.Errorf("invalid --session-id %q: must be a canonical UUID", rawID)
	}
	if err := chat.ValidateSessionID(rawID); err != nil {
		return "", fmt.Errorf("invalid --session-id: %w", err)
	}
	return rawID, nil
}

func validateSessionPersistenceFlags(disabled, continueLast bool, resumeID string, fork bool) error {
	if disabled && (continueLast || resumeID != "" || fork) {
		return fmt.Errorf("--no-session-persistence cannot be combined with --continue, --resume, or --fork-session")
	}
	return nil
}

func loadConfiguredConfig(path string) (*config.Config, error) {
	if strings.TrimSpace(path) == "" {
		return config.Load()
	}
	// Bind the whole process to this file before loading it. The setup wizard
	// and the "saved to <path>" messages resolve the config location on their
	// own; without this a first run with --config wrote the API key to the
	// DEFAULT config and then failed again on the explicit file that still had
	// no credentials.
	config.SetExplicitConfigPath(path)
	return config.LoadFrom(path)
}

func applyRunConfigOverrides(
	cfg *config.Config,
	runtimeVersion, providerName, modelName, customBaseURL string,
	disableSessionPersistence bool,
) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	cfg.Version = runtimeVersion
	if err := applyRuntimeOverrides(cfg, providerName, modelName); err != nil {
		return err
	}
	if err := applyRuntimeBaseURLOverride(cfg, customBaseURL); err != nil {
		return err
	}
	if disableSessionPersistence {
		// Runtime-only override: never Save the config. Disabling AutoLoad as
		// well keeps an interactive ephemeral run from restoring old history.
		cfg.Session.Enabled = false
		cfg.Session.AutoLoad = false
	}
	return nil
}

// applyBareRunConfig marks the in-memory config for runtime isolation. The
// builder/app enforce the overlay without mutating persisted feature fields,
// so a later /model or /settings save cannot accidentally disable the user's
// normal configuration.
func applyBareRunConfig(cfg *config.Config, enabled bool) {
	if cfg == nil || !enabled {
		return
	}
	cfg.Bare = true
}

type cliDebugFlags struct {
	debug        string
	debugSet     bool
	file         string
	debugFileSet bool
}

type resolvedCLIDebug struct {
	enabled bool
	path    string
	filter  string
	level   logging.Level
}

// normalizeOptionalDebugArgs gives Cobra's string flag the same optional
// separated value syntax as Claude Code: both `--debug` and
// `--debug "api,mcp"` work. A `--` delimiter remains authoritative.
func normalizeOptionalDebugArgs(args []string) []string {
	normalized := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			normalized = append(normalized, args[i:]...)
			break
		}
		if arg != "--debug" || i+1 >= len(args) ||
			args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
			normalized = append(normalized, arg)
			continue
		}
		normalized = append(normalized, "--debug="+args[i+1])
		i++
	}
	return normalized
}

func resolveCLIDebug(flags cliDebugFlags) (resolvedCLIDebug, error) {
	enabled := flags.debugSet || flags.debugFileSet
	if !enabled {
		return resolvedCLIDebug{}, nil
	}

	filter := strings.TrimSpace(flags.debug)
	if filter == "" {
		filter = "*"
	}

	path := ""
	// pathIsDirectory marks a value that names a DIRECTORY to write into rather
	// than the log file itself.
	pathIsDirectory := false
	if flags.debugFileSet {
		path = strings.TrimSpace(flags.file)
		if path == "" {
			return resolvedCLIDebug{}, errors.New("--debug-file requires a non-empty path")
		}
	} else {
		path = strings.TrimSpace(os.Getenv("GOKIN_DEBUG_LOG_FILE"))
		if path == "" {
			// Claude-compatible environment alias. Like Claude Code, the
			// variable selects a destination but does not enable debug by
			// itself — and, as its name says, that destination is a DIRECTORY.
			// Treating it as a file made `--debug` fail outright for anyone who
			// already exports it for Claude Code.
			path = strings.TrimSpace(os.Getenv("CLAUDE_CODE_DEBUG_LOGS_DIR"))
			pathIsDirectory = path != ""
		}
	}
	if strings.ContainsRune(path, '\x00') {
		return resolvedCLIDebug{}, errors.New("debug log path contains NUL")
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return resolvedCLIDebug{}, fmt.Errorf("expand debug log path: %w", err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	if !pathIsDirectory && path != "" {
		// An explicit path that already exists as a directory is also a
		// destination directory, not a file to truncate.
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			pathIsDirectory = true
		}
	}
	if path == "" || pathIsDirectory {
		directory := path
		if directory == "" {
			configDir, err := appcontext.GetConfigDir()
			if err != nil {
				return resolvedCLIDebug{}, fmt.Errorf("resolve default debug directory: %w", err)
			}
			directory = filepath.Join(configDir, "debug")
		}
		name := fmt.Sprintf("gokin-%s-%d.jsonl",
			time.Now().UTC().Format("20060102T150405.000000000Z"), os.Getpid())
		path = filepath.Join(directory, name)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return resolvedCLIDebug{}, fmt.Errorf("resolve debug log path: %w", err)
	}

	levelName := strings.TrimSpace(os.Getenv("GOKIN_DEBUG_LOG_LEVEL"))
	if levelName == "" {
		levelName = strings.TrimSpace(os.Getenv("CLAUDE_CODE_DEBUG_LOG_LEVEL"))
	}
	if levelName == "" {
		levelName = "debug"
	}
	var level logging.Level
	switch strings.ToLower(levelName) {
	case "verbose", "debug":
		level = logging.LevelDebug
	case "info":
		level = logging.LevelInfo
	case "warn", "warning":
		level = logging.LevelWarn
	case "error":
		level = logging.LevelError
	default:
		return resolvedCLIDebug{}, fmt.Errorf(
			"invalid debug log level %q (want verbose, debug, info, warn, or error)",
			levelName)
	}

	return resolvedCLIDebug{
		enabled: true,
		path:    absolute,
		filter:  filter,
		level:   level,
	}, nil
}

func applyDebugRunConfig(cfg *config.Config, debug resolvedCLIDebug) {
	if cfg == nil || !debug.enabled {
		return
	}
	cfg.Debug = true
	cfg.DebugFile = debug.path
	cfg.DebugFilter = debug.filter
	cfg.DebugLevel = string(debug.level)
}

// configureBareEnvironment mirrors Claude Code's CLAUDE_CODE_SIMPLE signal
// for child processes launched through Bash. Restore it after this invocation
// so in-process command tests and embedders do not inherit global state.
func configureBareEnvironment(enabled bool) (func(), error) {
	if !enabled {
		return func() {}, nil
	}
	previous, existed := os.LookupEnv("CLAUDE_CODE_SIMPLE")
	if err := os.Setenv("CLAUDE_CODE_SIMPLE", "1"); err != nil {
		return nil, fmt.Errorf("set CLAUDE_CODE_SIMPLE for --bare: %w", err)
	}
	return func() {
		if existed {
			_ = os.Setenv("CLAUDE_CODE_SIMPLE", previous)
		} else {
			_ = os.Unsetenv("CLAUDE_CODE_SIMPLE")
		}
	}, nil
}

type resumableApplication interface {
	GetSession() *chat.Session
	SelectNewSessionID(sessionID string) error
	ResumeLastSession() error
	ResumeSession(sessionID string) error
	ForkLoadedSession(sessionID string) error
}

var errSessionIDInUse = errors.New("session ID is already in use")

func prepareSessionForRun(
	application resumableApplication,
	persistenceEnabled bool,
	exactID string,
	continueLast bool,
	requestedNewID string,
	fork bool,
) (*chat.SessionWriterLease, string, error) {
	if application == nil || application.GetSession() == nil {
		return nil, exactID, fmt.Errorf("session runtime is not initialized")
	}
	if (exactID != "" || continueLast) && !persistenceEnabled {
		return nil, exactID, fmt.Errorf("cannot resume: session persistence is disabled")
	}

	resuming := exactID != "" || continueLast
	if !resuming {
		selectedID := application.GetSession().GetID()
		if requestedNewID != "" {
			selectedID = requestedNewID
		}
		if !persistenceEnabled {
			if requestedNewID != "" {
				if err := application.SelectNewSessionID(selectedID); err != nil {
					return nil, selectedID, fmt.Errorf("select new session ID %q: %w", selectedID, err)
				}
			}
			return nil, selectedID, nil
		}
		lease, err := chat.AcquireSessionWriterLease(selectedID)
		if err != nil {
			return nil, selectedID, fmt.Errorf("acquire writer lease for session %q: %w", selectedID, err)
		}
		if requestedNewID != "" {
			if err := ensureSessionIDAvailable(selectedID); err != nil {
				_ = lease.Release()
				return nil, selectedID, err
			}
			if err := application.SelectNewSessionID(selectedID); err != nil {
				_ = lease.Release()
				return nil, selectedID, fmt.Errorf("select new session ID %q: %w", selectedID, err)
			}
		}
		return lease, selectedID, nil
	}

	selectedID := exactID
	if continueLast {
		// LoadLast selects the newest usable snapshot. No model/tool call can
		// occur here. After acquiring its exact ID below we reload that snapshot
		// under the lease, closing the selection/acquisition TOCTOU window.
		if err := application.ResumeLastSession(); err != nil {
			return nil, "", fmt.Errorf("continue last session: %w", err)
		}
		selectedID = application.GetSession().GetID()
	}
	sourceLease, err := chat.AcquireSessionWriterLease(selectedID)
	if err != nil {
		return nil, selectedID, fmt.Errorf("acquire writer lease for session %q: %w", selectedID, err)
	}

	if err := application.ResumeSession(selectedID); err != nil {
		_ = sourceLease.Release()
		return nil, selectedID, fmt.Errorf("resume session %q: %w", selectedID, err)
	}
	if !fork {
		return sourceLease, selectedID, nil
	}

	forkLease, forkID, err := acquireFreshForkSessionLease()
	if err != nil {
		_ = sourceLease.Release()
		return nil, selectedID, err
	}
	if err := application.ForkLoadedSession(forkID); err != nil {
		_ = forkLease.Release()
		_ = sourceLease.Release()
		return nil, forkID, fmt.Errorf("fork resumed session %q: %w", selectedID, err)
	}
	if err := sourceLease.Release(); err != nil {
		logging.Warn("failed to release source session writer lease after fork",
			"source_session_id", selectedID,
			"session_id", forkID,
			"error", err)
	}
	return forkLease, forkID, nil
}

func ensureSessionIDAvailable(sessionID string) error {
	history, err := chat.NewHistoryManager()
	if err != nil {
		return fmt.Errorf("check session ID %q availability: %w", sessionID, err)
	}
	if _, err := history.LoadFull(sessionID); err == nil {
		return fmt.Errorf("%w: %q already has persisted state; use --resume instead", errSessionIDInUse, sessionID)
	} else if !errors.Is(err, os.ErrNotExist) {
		// Corrupt files, symlinks, directories, and unreadable entries all own
		// their pathname. Never "repair" them by overwriting from a new run.
		return fmt.Errorf("cannot safely establish availability of session ID %q: %w", sessionID, err)
	}
	return nil
}

func acquireFreshForkSessionLease() (*chat.SessionWriterLease, string, error) {
	const attempts = 8
	for range attempts {
		sessionID := uuid.NewString()
		lease, err := chat.AcquireSessionWriterLease(sessionID)
		if err != nil {
			if errors.Is(err, chat.ErrSessionWriterLeaseBusy) {
				continue
			}
			return nil, sessionID, fmt.Errorf("acquire writer lease for fork session %q: %w", sessionID, err)
		}
		if err := ensureSessionIDAvailable(sessionID); err != nil {
			_ = lease.Release()
			if errors.Is(err, errSessionIDInUse) {
				continue
			}
			return nil, sessionID, err
		}
		return lease, sessionID, nil
	}
	return nil, "", fmt.Errorf("could not allocate an unused fork session UUID after %d attempts", attempts)
}

func sessionPreparationErrorKind(err error, resuming bool) string {
	if loadKind, ok := chat.SessionLoadErrorKindOf(err); ok {
		return "session_" + string(loadKind)
	}
	if errors.Is(err, app.ErrSessionProviderMismatch) {
		return "session_provider_mismatch"
	}
	if errors.Is(err, chat.ErrSessionWriterLeaseBusy) {
		return "session_busy"
	}
	if !resuming {
		return "session_lease"
	}
	return "resume_failed"
}

func writeHeadlessFailure(w io.Writer, sessionID, kind string, failure error) error {
	if w == nil {
		return fmt.Errorf("write headless JSON failure: output is nil")
	}
	if failure == nil {
		return fmt.Errorf("write headless JSON failure: failure is nil")
	}
	if strings.TrimSpace(kind) == "" {
		kind = "startup"
	}
	result := app.HeadlessResult{
		SchemaVersion: app.HeadlessSchemaVersion,
		Type:          "result",
		Result:        "",
		SessionID:     sessionID,
		Status:        "error",
		Error: &app.HeadlessError{
			Kind:    kind,
			Message: failure.Error(),
		},
		Usage: app.HeadlessUsage{},
		Cost:  app.HeadlessCost{},
	}
	if err := json.NewEncoder(w).Encode(result); err != nil {
		return fmt.Errorf("write headless JSON failure: %w", err)
	}
	return nil
}

// applyAddDirFlags resolves each --add-dir value, refuses ungrantable locations
// (filesystem root, system dirs, .git, secret dirs), and appends it to
// cfg.Tools.AllowedDirs IN MEMORY ONLY (never saved). The builder propagates
// AllowedDirs to every path-scoping tool and seeds the agent runner at boot, so
// these grants are live before the first prompt — interactive, headless, or eval.
func applyAddDirFlags(cfg *config.Config, dirs []string) error {
	for _, raw := range dirs {
		d := strings.TrimSpace(raw)
		if d == "" {
			continue
		}
		if d == "~" || strings.HasPrefix(d, "~/") {
			if home, err := os.UserHomeDir(); err == nil && home != "" {
				if d == "~" {
					d = home
				} else {
					d = filepath.Join(home, d[2:])
				}
			}
		}
		abs, err := filepath.Abs(d)
		if err != nil {
			return fmt.Errorf("--add-dir %q: %w", raw, err)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return fmt.Errorf("--add-dir %q: %v", raw, statErr)
		}
		if !info.IsDir() {
			return fmt.Errorf("--add-dir %q: not a directory", raw)
		}
		if resolved, rErr := filepath.EvalSymlinks(abs); rErr == nil {
			abs = resolved
		}
		if err := security.IsGrantableDir(abs); err != nil {
			return fmt.Errorf("--add-dir %q: %v", raw, err)
		}
		cfg.AddAllowedDir(abs) // dedups; in-memory only (no Save)
	}
	return nil
}

func applyRuntimeOverrides(cfg *config.Config, providerOverride, modelOverride string) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}

	providerOverride = strings.ToLower(strings.TrimSpace(providerOverride))
	modelOverride = strings.TrimSpace(modelOverride)

	if providerOverride != "" {
		p := config.GetProvider(providerOverride)
		if p == nil {
			return fmt.Errorf("unknown provider %q (supported: %s)", providerOverride, strings.Join(config.ProviderNames(), ", "))
		}
		cfg.API.ActiveProvider = providerOverride
		cfg.API.Backend = providerOverride
		cfg.Model.Provider = providerOverride
		if modelOverride == "" && p.DefaultModel != "" {
			cfg.Model.Name = p.DefaultModel
		}
	}

	if modelOverride != "" {
		cfg.Model.Name = modelOverride
		if providerOverride == "" {
			if detected := config.DetectKnownProviderFromModel(modelOverride); detected != "" {
				cfg.Model.Provider = detected
				cfg.API.ActiveProvider = detected
				cfg.API.Backend = detected
			}
		}
	}

	return nil
}

func applyRuntimeBaseURLOverride(cfg *config.Config, raw string) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid --base-url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid --base-url %q: must be an absolute http/https URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("invalid --base-url %q: credentials, query, and fragment are not allowed", raw)
	}
	u.Path = strings.TrimRight(u.Path, "/")
	cfg.Model.CustomBaseURL = u.String()
	return nil
}
