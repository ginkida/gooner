package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"gokin/internal/app"
	backgroundstore "gokin/internal/background"
	"gokin/internal/chat"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const backgroundJobEnv = "GOKIN_BACKGROUND_JOB_ID"

type backgroundWorkerContext struct {
	id    string
	store *backgroundstore.Store
	lease *backgroundstore.WorkerLease
}

type steerableHeadlessRunner interface {
	headlessInputRunner
	TrySteerHeadless(message string) bool
}

func beginBackgroundWorker() (*backgroundWorkerContext, error) {
	id := strings.TrimSpace(os.Getenv(backgroundJobEnv))
	if id == "" {
		return nil, nil
	}
	// A nested gokin launched by this worker is a normal independent process,
	// not a second owner of the same durable job.
	_ = os.Unsetenv(backgroundJobEnv)
	store, err := backgroundstore.NewStore()
	if err != nil {
		return nil, fmt.Errorf("open background job store: %w", err)
	}
	// Retry briefly: liveness probes (`gokin agents`, `gokin stop`) take this
	// same exclusive lock for a moment, so a single non-blocking attempt could
	// lose a race with a poll and make this worker fail to claim its own job.
	lease, err := store.AcquireWorkerLeaseWithin(id, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("acquire background worker lease: %w", err)
	}
	if _, err := store.MarkRunning(id, os.Getpid()); err != nil {
		_ = lease.Release()
		return nil, fmt.Errorf("publish background worker state: %w", err)
	}
	return &backgroundWorkerContext{id: id, store: store, lease: lease}, nil
}

func (w *backgroundWorkerContext) setSessionID(sessionID string) error {
	if w == nil || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	_, err := w.store.SetSessionID(w.id, sessionID)
	return err
}

func (w *backgroundWorkerContext) finish(runErr error) {
	if w == nil {
		return
	}
	state, exitCode := backgroundstore.StateSucceeded, 0
	if runErr != nil {
		state, exitCode = backgroundstore.StateFailed, 1
	}
	if _, err := w.store.Finish(w.id, state, exitCode); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to finalize background job metadata: %v\n", err)
	}
	if err := w.lease.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to release background worker lease: %v\n", err)
	}
}

func runBackgroundHeadlessLoop(
	ctx context.Context,
	runner steerableHeadlessRunner,
	initialPrompt string,
	opts app.HeadlessOptions,
	worker *backgroundWorkerContext,
) error {
	if runner == nil || worker == nil {
		return fmt.Errorf("background headless runtime is not initialized")
	}
	if opts.StreamState == nil {
		opts.StreamState = app.NewHeadlessStreamState()
	}
	opts.InlineExternalSteers = true
	prompt := initialPrompt
	var claimedForTurn *backgroundstore.Control

	for {
		type turnResult struct {
			err error
		}
		done := make(chan turnResult, 1)
		turnCtx, cancelTurn := context.WithCancel(ctx)
		go func(turnPrompt string) {
			_, err := runner.RunHeadlessWithOptions(turnCtx, turnPrompt, opts)
			done <- turnResult{err: err}
		}(prompt)

		ticker := time.NewTicker(150 * time.Millisecond)
		var steered *backgroundstore.Control
		var deferred *backgroundstore.Control
		var result turnResult
		turnFinished := false
		for !turnFinished {
			select {
			case result = <-done:
				turnFinished = true
			case <-ctx.Done():
				// Wait for RunHeadless to observe the same cancellation before
				// releasing App/session ownership. Claimed input intentionally
				// remains ambiguous across an externally-killed worker.
				result = <-done
				turnFinished = true
			case <-ticker.C:
				// claimedForTurn is the control that BECAME this turn's prompt.
				// It stays in state `claimed` until the turn commits it below,
				// so polling the inbox now would find our own record and report
				// it as ambiguous delivery — killing the worker ~150ms into
				// every continuation turn. New input that arrives meanwhile is
				// still delivered: the post-turn drain picks it up as the next
				// turn.
				if steered != nil || deferred != nil || claimedForTurn != nil {
					continue
				}
				control, err := worker.store.ClaimNextControl(worker.id)
				if err != nil {
					ticker.Stop()
					// The turn is still running. Cancel it and wait, exactly as
					// the ctx.Done() branch does — returning here would release
					// App/session/job ownership out from under a live turn.
					cancelTurn()
					<-done
					return fmt.Errorf("claim background control input: %w", err)
				}
				if control == nil {
					continue
				}
				if runner.TrySteerHeadless(control.Message) {
					steered = control
				} else {
					deferred = control
				}
			}
		}
		ticker.Stop()
		// The turn goroutine has returned on every path that reaches here, so
		// releasing its context now keeps one derived context per iteration
		// from accumulating for the life of the worker.
		cancelTurn()

		outcome := "completed"
		if result.err != nil {
			outcome = "turn_failed"
		}
		if claimedForTurn != nil {
			if err := worker.store.CompleteControl(*claimedForTurn, outcome); err != nil {
				return fmt.Errorf("complete background next-turn input: %w", err)
			}
			claimedForTurn = nil
		}
		if steered != nil {
			steerOutcome := "steered"
			if result.err != nil {
				steerOutcome = "steered_turn_failed"
			}
			if err := worker.store.CompleteControl(*steered, steerOutcome); err != nil {
				return fmt.Errorf("complete background steered input: %w", err)
			}
		}
		if result.err != nil {
			return result.err
		}
		if deferred != nil {
			claimedForTurn = deferred
			prompt = deferred.Message
			continue
		}

		// Close the completion/arrival race: after the turn's claimed steer is
		// committed, one final inbox read decides whether the worker is done.
		next, err := worker.store.ClaimNextControl(worker.id)
		if err != nil {
			return fmt.Errorf("claim queued background follow-up: %w", err)
		}
		if next != nil {
			claimedForTurn = next
			prompt = next.Message
			continue
		}
		finishing, err := worker.store.BeginFinishing(worker.id)
		if err != nil {
			return fmt.Errorf("close background input admission: %w", err)
		}
		if finishing {
			return nil
		}
		next, err = worker.store.ClaimNextControl(worker.id)
		if err != nil {
			return fmt.Errorf("claim background follow-up after finish race: %w", err)
		}
		if next == nil {
			return fmt.Errorf("background inbox changed while closing input admission")
		}
		claimedForTurn = next
		prompt = next.Message
	}
}

func (w *backgroundWorkerContext) installSignalContext(cmd *cobra.Command) func() {
	if w == nil || cmd == nil {
		return func() {}
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	cmd.SetContext(ctx)
	return stop
}

func launchBackgroundSession(cmd *cobra.Command, args []string) error {
	if headless || printMode {
		return fmt.Errorf("--background cannot be combined with --print or --headless")
	}
	if noSessionPersistence {
		return fmt.Errorf("--background requires session persistence")
	}
	if inputFormat != "" && inputFormat != string(headlessInputText) {
		return fmt.Errorf("--background supports text prompts only")
	}
	if runSetup {
		return fmt.Errorf("--background cannot run the setup wizard")
	}
	if _, err := resolveInteractivePrompt(prompt, args); err != nil {
		return err
	}
	if strings.TrimSpace(prompt) == "" && strings.TrimSpace(strings.Join(args, " ")) == "" {
		return fmt.Errorf("--background requires a prompt; piped stdin is not detached")
	}
	if len(rootInvocationArgs) == 0 {
		return fmt.Errorf("background launch arguments are unavailable")
	}
	// The detached child re-parses these same flags. Without this preflight a
	// deterministically invalid invocation still printed "Started …" and exited
	// 0, and the failure only surfaced later inside the job's log — the
	// launcher reported success for a run that could never happen.
	if err := preflightBackgroundLaunch(cmd); err != nil {
		return err
	}

	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve background work directory: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("resolve absolute background work directory: %w", err)
	}
	store, err := backgroundstore.NewStore()
	if err != nil {
		return fmt.Errorf("open background job store: %w", err)
	}
	initialSessionID := strings.TrimSpace(sessionID)
	childArgs := backgroundChildArgs(rootInvocationArgs)
	if initialSessionID == "" && resumeSession == "" && !continueLast {
		initialSessionID = uuid.NewString()
		childArgs = appendRootFlags(childArgs, "--session-id", initialSessionID)
	}
	job, err := startDetachedBackgroundJob(store, workDir, initialSessionID, "", childArgs)
	if err != nil {
		return err
	}

	printBackgroundStarted(cmd.OutOrStdout(), "Started", job, "")
	return nil
}

// preflightBackgroundLaunch runs the deterministic CLI validation the detached
// worker will run, so a typo fails at the launcher instead of becoming a
// "Started" job that immediately dies. It deliberately reuses runApp's own
// validators rather than restating the rules, so the two paths cannot drift.
// Anything environment-dependent (credentials, config load, app construction)
// stays the child's job — it may legitimately differ by the time it runs.
func preflightBackgroundLaunch(cmd *cobra.Command) error {
	flagChanged := func(name string, nonEmptyFallback bool) bool {
		if cmd == nil {
			return nonEmptyFallback
		}
		return cmd.Flags().Changed(name)
	}
	if err := validateHeadlessExecutionLimits(true, headlessTurns, headlessLimit, headlessBudget); err != nil {
		return err
	}
	if _, err := resolveCLIPermissionMode(permissionMode, skipPermissions); err != nil {
		return err
	}
	if _, err := resolveCLIDebug(cliDebugFlags{
		debug:        debugCategories,
		debugSet:     flagChanged("debug", debugCategories != ""),
		file:         debugFile,
		debugFileSet: flagChanged("debug-file", debugFile != ""),
	}); err != nil {
		return err
	}
	// A detached worker always emits stream-json, so validate the schema
	// against the format the child will actually run with.
	if _, err := resolveCLIJSONSchema(
		true, app.HeadlessOutputStreamJSON, jsonSchema, flagChanged("json-schema", jsonSchema != ""),
	); err != nil {
		return err
	}
	if _, err := resolveCLISystemPrompt(cliSystemPromptFlags{
		replacement:     systemPrompt,
		replacementSet:  flagChanged("system-prompt", systemPrompt != ""),
		replacementFile: systemPromptFile,
		fileSet:         flagChanged("system-prompt-file", systemPromptFile != ""),
		append:          appendSystemPrompt,
		appendSet:       flagChanged("append-system-prompt", appendSystemPrompt != ""),
		appendFile:      appendSystemFile,
		appendFileSet:   flagChanged("append-system-prompt-file", appendSystemFile != ""),
	}); err != nil {
		return err
	}
	if _, err := resolveCLIAllowedToolRules(append(
		append([]string(nil), allowedTools...), allowedToolsCompat...,
	)); err != nil {
		return err
	}
	if _, err := resolveCLIDeniedToolRules(append(
		append([]string(nil), deniedTools...), deniedToolsCompat...,
	)); err != nil {
		return err
	}
	resumeID, err := validateResumeSelection(continueLast, resumeSession)
	if err != nil {
		return err
	}
	if _, err := validateNewSessionSelection(sessionID, continueLast, resumeID, forkSession); err != nil {
		return err
	}
	return validateSessionPersistenceFlags(noSessionPersistence, continueLast, resumeID, forkSession)
}

func startDetachedBackgroundJob(
	store *backgroundstore.Store,
	workDir, initialSessionID, parentJobID string,
	childArgs []string,
) (backgroundstore.Job, error) {
	if store == nil {
		return backgroundstore.Job{}, fmt.Errorf("background store is not initialized")
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return backgroundstore.Job{}, fmt.Errorf("inspect background work directory: %w", err)
	}
	if !info.IsDir() {
		return backgroundstore.Job{}, fmt.Errorf("background work directory %q is not a directory", workDir)
	}
	if len(childArgs) == 0 {
		return backgroundstore.Job{}, fmt.Errorf("background worker arguments are empty")
	}
	jobID := backgroundstore.NewJobID()
	job := backgroundstore.Job{
		ID:          jobID,
		ParentJobID: parentJobID,
		SessionID:   initialSessionID,
		State:       backgroundstore.StateStarting,
		WorkDir:     workDir,
		StartedAt:   time.Now(),
	}
	if err := store.Create(job); err != nil {
		return backgroundstore.Job{}, fmt.Errorf("create background job: %w", err)
	}
	stdoutPath, _ := store.StdoutPath(jobID)
	stderrPath, _ := store.StderrPath(jobID)
	stdout, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_, _ = store.Finish(jobID, backgroundstore.StateFailed, 1)
		return backgroundstore.Job{}, fmt.Errorf("create background stdout log: %w", err)
	}
	stderr, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdout.Close()
		_, _ = store.Finish(jobID, backgroundstore.StateFailed, 1)
		return backgroundstore.Job{}, fmt.Errorf("create background stderr log: %w", err)
	}
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		_, _ = store.Finish(jobID, backgroundstore.StateFailed, 1)
		return backgroundstore.Job{}, fmt.Errorf("open null input for background worker: %w", err)
	}

	executable, err := os.Executable()
	if err != nil {
		_ = devNull.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		_, _ = store.Finish(jobID, backgroundstore.StateFailed, 1)
		return backgroundstore.Job{}, fmt.Errorf("resolve current executable: %w", err)
	}
	child := exec.Command(executable, childArgs...)
	child.Dir = workDir
	child.Stdin = devNull
	child.Stdout = stdout
	child.Stderr = stderr
	child.Env = replaceEnvironmentValue(os.Environ(), backgroundJobEnv, jobID)
	configureDetachedProcess(child)
	startErr := child.Start()
	_ = devNull.Close()
	_ = stdout.Close()
	_ = stderr.Close()
	if startErr != nil {
		_, _ = store.Finish(jobID, backgroundstore.StateFailed, 1)
		return backgroundstore.Job{}, fmt.Errorf("start background worker: %w", startErr)
	}
	_ = child.Process.Release()
	return job, nil
}

func printBackgroundStarted(w io.Writer, verb string, job backgroundstore.Job, sourceID string) {
	shortID := job.ID[:8]
	lineage := ""
	if sourceID != "" {
		lineage = fmt.Sprintf(" from %s", sourceID[:8])
	}
	fmt.Fprintf(w,
		"%s background session %s%s\nJob ID: %s\nLogs: gokin logs %s --follow\nStop: gokin stop %s\n",
		verb, shortID, lineage, job.ID, shortID, shortID)
}

func backgroundChildArgs(raw []string) []string {
	out := make([]string, 0, len(raw)+4)
	for i := 0; i < len(raw); i++ {
		arg := raw[i]
		if arg == "--" {
			out = append(out, raw[i:]...)
			break
		}
		switch {
		case arg == "--background" || arg == "--bg" ||
			strings.HasPrefix(arg, "--background=") || strings.HasPrefix(arg, "--bg="):
			continue
		case arg == "--print" || arg == "-p" || arg == "--headless":
			continue
		case arg == "--output-format" || arg == "--input-format":
			i++
			continue
		case strings.HasPrefix(arg, "--output-format=") || strings.HasPrefix(arg, "--input-format="):
			continue
		default:
			out = append(out, arg)
		}
	}
	return appendRootFlags(out, "--print", "--output-format", "stream-json", "--input-format", "text")
}

func appendRootFlags(args []string, flags ...string) []string {
	for i, arg := range args {
		if arg == "--" {
			out := make([]string, 0, len(args)+len(flags))
			out = append(out, args[:i]...)
			out = append(out, flags...)
			out = append(out, args[i:]...)
			return out
		}
	}
	return append(args, flags...)
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func newBackgroundAgentsCmd() *cobra.Command {
	var asJSON, includeCompleted bool
	var cwd string
	command := &cobra.Command{
		Use:   "agents",
		Short: "List detached Gokin sessions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := backgroundstore.NewStore()
			if err != nil {
				return err
			}
			if cwd == "" {
				cwd, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			cwd, err = filepath.Abs(cwd)
			if err != nil {
				return err
			}
			jobs, err := store.List(cwd, includeCompleted)
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(jobs)
			}
			if len(jobs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No background sessions.")
				return nil
			}
			for _, job := range jobs {
				session := job.SessionID
				if session == "" {
					session = "-"
				}
				input := ""
				if job.PendingInput > 0 || job.AmbiguousInput > 0 {
					input = fmt.Sprintf(" input=pending:%d,ambiguous:%d", job.PendingInput, job.AmbiguousInput)
				}
				parent := ""
				if job.ParentJobID != "" {
					parent = " parent=" + job.ParentJobID[:8]
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %-11s pid=%-7d session=%s%s%s  started=%s\n",
					job.ID[:8], job.State, job.PID, session, parent, input, job.StartedAt.Format(time.RFC3339))
			}
			return nil
		},
	}
	command.Flags().BoolVar(&asJSON, "json", false, "print jobs as JSON")
	command.Flags().BoolVar(&includeCompleted, "all", false, "include completed jobs")
	command.Flags().StringVar(&cwd, "cwd", "", "filter by working directory (default current directory)")
	return command
}

func newBackgroundLogsCmd() *cobra.Command {
	var follow bool
	var lines int
	command := &cobra.Command{
		Use:   "logs <id>",
		Short: "Print output from a detached Gokin session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 1 || lines > 10_000 {
				return fmt.Errorf("--lines must be between 1 and 10000")
			}
			store, err := backgroundstore.NewStore()
			if err != nil {
				return err
			}
			job, err := resolveBackgroundJob(store, args[0])
			if err != nil {
				return err
			}
			if follow {
				return followBackgroundLogs(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), store, job)
			}
			stdoutPath, _ := store.StdoutPath(job.ID)
			stderrPath, _ := store.StderrPath(job.ID)
			if err := printTail(cmd.OutOrStdout(), stdoutPath, lines); err != nil {
				return err
			}
			return printTail(cmd.ErrOrStderr(), stderrPath, lines)
		},
	}
	command.Flags().BoolVarP(&follow, "follow", "f", false, "follow output until the job exits")
	command.Flags().IntVarP(&lines, "lines", "n", 200, "number of recent lines to print")
	return command
}

func newBackgroundStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "stop <id>",
		Aliases: []string{"kill"},
		Short:   "Stop a detached Gokin session",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := backgroundstore.NewStore()
			if err != nil {
				return err
			}
			job, err := resolveBackgroundJob(store, args[0])
			if err != nil {
				return err
			}
			// The launcher returns as soon as exec succeeds; give the worker a
			// short window to acquire its lease and publish its PID so an
			// immediate `gokin stop <id>` is reliable rather than racy.
			deadline := time.Now().Add(2 * time.Second)
			for {
				job, err = store.Reconcile(job)
				if err != nil {
					return err
				}
				if job.Terminal() {
					return fmt.Errorf("background job %s is already %s", job.ID[:8], job.State)
				}
				held, probeErr := store.WorkerLeaseHeld(job.ID)
				if probeErr != nil {
					return probeErr
				}
				if held && job.PID > 0 {
					break
				}
				if job.State != backgroundstore.StateStarting || time.Now().After(deadline) {
					return fmt.Errorf("background job %s has no live worker", job.ID[:8])
				}
				time.Sleep(25 * time.Millisecond)
				job, err = store.Load(job.ID)
				if err != nil {
					return err
				}
			}
			if _, err := store.MarkStopping(job.ID); err != nil {
				return err
			}
			if err := stopDetachedProcess(job.PID); err != nil {
				return fmt.Errorf("stop background job %s: %w", job.ID[:8], err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stopping background session %s (pid %d).\n", job.ID[:8], job.PID)
			return nil
		},
	}
}

func newBackgroundRespawnCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "respawn <id> <prompt>",
		Short: "Continue a completed detached session as a new background job",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			message := strings.TrimSpace(strings.Join(args[1:], " "))
			if message == "" {
				return fmt.Errorf("respawn prompt is empty")
			}
			if strings.HasPrefix(message, "/") {
				return fmt.Errorf("slash commands are not accepted by detached headless sessions")
			}
			if err := validateRespawnInvocation(cmd); err != nil {
				return err
			}
			store, err := backgroundstore.NewStore()
			if err != nil {
				return err
			}
			source, err := resolveBackgroundJob(store, args[0])
			if err != nil {
				return err
			}
			source, err = store.Reconcile(source)
			if err != nil {
				return err
			}
			source, err = store.RefreshControlCounts(source)
			if err != nil {
				return err
			}
			if !source.Terminal() {
				return fmt.Errorf("background job %s is still %s; use gokin send or attach instead",
					source.ID[:8], source.State)
			}
			if source.SessionID == "" {
				return fmt.Errorf("background job %s has no resumable session", source.ID[:8])
			}
			if source.PendingInput > 0 || source.AmbiguousInput > 0 {
				return fmt.Errorf(
					"background job %s has unresolved input (pending=%d, ambiguous=%d); review it before respawning",
					source.ID[:8], source.PendingInput, source.AmbiguousInput)
			}

			// Load provider identity from a consistent snapshot. Releasing this
			// probe before starting the child is necessary because the worker
			// becomes the long-lived owner; its normal writer-lease acquisition
			// remains the authoritative protection against a later race.
			lease, err := chat.AcquireSessionWriterLease(source.SessionID)
			if err != nil {
				return fmt.Errorf("background session %s cannot be respawned: %w", source.ID[:8], err)
			}
			history, historyErr := chat.NewHistoryManager()
			var persisted *chat.SessionState
			if historyErr == nil {
				persisted, historyErr = history.LoadFull(source.SessionID)
			}
			releaseErr := lease.Release()
			if historyErr != nil {
				return fmt.Errorf("load background session %s for respawn: %w", source.ID[:8], historyErr)
			}
			if releaseErr != nil {
				return fmt.Errorf("release respawn session probe: %w", releaseErr)
			}
			if persisted.Provider != "" && provider != "" &&
				!strings.EqualFold(strings.TrimSpace(persisted.Provider), strings.TrimSpace(provider)) {
				return fmt.Errorf(
					"session provider mismatch: background session %s uses %s, not %s",
					source.ID[:8], persisted.Provider, provider)
			}
			childArgs, err := respawnChildArgs(cmd, source.SessionID, persisted.Provider, message)
			if err != nil {
				return err
			}
			initialSessionID := source.SessionID
			if forkSession {
				initialSessionID = ""
			}
			job, err := startDetachedBackgroundJob(
				store, source.WorkDir, initialSessionID, source.ID, childArgs)
			if err != nil {
				return err
			}
			printBackgroundStarted(cmd.OutOrStdout(), "Respawned", job, source.ID)
			return nil
		},
	}
}

func validateRespawnInvocation(cmd *cobra.Command) error {
	for _, name := range []string{
		"background", "bg", "headless", "print", "continue", "resume",
		"session-id", "no-session-persistence", "setup", "prompt",
	} {
		flag := explicitRespawnFlag(cmd, name)
		if flag != nil && flag.Changed {
			return fmt.Errorf("respawn cannot be combined with --%s", name)
		}
	}
	return nil
}

func respawnChildArgs(
	cmd *cobra.Command,
	sessionID, sessionProvider, message string,
) ([]string, error) {
	if cmd == nil || cmd.Root() == nil {
		return nil, fmt.Errorf("respawn command is not initialized")
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("respawn session ID is empty")
	}
	if strings.TrimSpace(message) == "" {
		return nil, fmt.Errorf("respawn prompt is empty")
	}
	excluded := map[string]bool{
		"background": true, "bg": true, "headless": true, "print": true,
		"continue": true, "resume": true, "session-id": true,
		"no-session-persistence": true, "setup": true, "prompt": true,
		"output-format": true, "input-format": true,
	}
	var args []string
	var encodeErr error
	visitExplicitRespawnFlags(cmd, func(flag *pflag.Flag) {
		if encodeErr != nil || excluded[flag.Name] {
			return
		}
		values := []string{flag.Value.String()}
		if slice, ok := flag.Value.(pflag.SliceValue); ok {
			values = slice.GetSlice()
		}
		for _, value := range values {
			value, encodeErr = respawnFlagValue(flag.Name, flag.Value.Type(), value)
			if encodeErr != nil {
				return
			}
			args = append(args, "--"+flag.Name+"="+value)
		}
	})
	if encodeErr != nil {
		return nil, encodeErr
	}
	providerFlag := explicitRespawnFlag(cmd, "provider")
	if strings.TrimSpace(sessionProvider) != "" && (providerFlag == nil || !providerFlag.Changed) {
		args = append(args, "--provider", strings.TrimSpace(sessionProvider))
	}
	args = append(args,
		"--resume", sessionID,
		"--print",
		"--output-format", "stream-json",
		"--input-format", "text",
		"--", message,
	)
	return args, nil
}

func visitExplicitRespawnFlags(cmd *cobra.Command, visit func(*pflag.Flag)) {
	if cmd == nil || cmd.Root() == nil || visit == nil {
		return
	}
	rootFlags := cmd.Root().PersistentFlags()
	seen := make(map[string]bool)
	for _, flags := range []*pflag.FlagSet{cmd.Flags(), cmd.InheritedFlags(), rootFlags} {
		flags.Visit(func(flag *pflag.Flag) {
			if seen[flag.Name] || rootFlags.Lookup(flag.Name) == nil {
				return
			}
			seen[flag.Name] = true
			visit(flag)
		})
	}
}

func explicitRespawnFlag(cmd *cobra.Command, name string) *pflag.Flag {
	if cmd == nil || cmd.Root() == nil {
		return nil
	}
	var fallback *pflag.Flag
	for _, flags := range []*pflag.FlagSet{
		cmd.Flags(), cmd.InheritedFlags(), cmd.Root().PersistentFlags(),
	} {
		flag := flags.Lookup(name)
		if flag == nil {
			continue
		}
		if fallback == nil {
			fallback = flag
		}
		if flag.Changed {
			return flag
		}
	}
	return fallback
}

func respawnFlagValue(name, flagType, value string) (string, error) {
	switch name {
	case "config", "system-prompt-file", "append-system-prompt-file", "debug-file", "add-dir":
		if strings.TrimSpace(value) != "" {
			absolute, err := filepath.Abs(value)
			if err != nil {
				return "", fmt.Errorf("resolve --%s path for respawn: %w", name, err)
			}
			value = absolute
		}
	}
	if flagType != "stringSlice" {
		return value, nil
	}
	var encoded strings.Builder
	writer := csv.NewWriter(&encoded)
	if err := writer.Write([]string{value}); err != nil {
		return "", fmt.Errorf("encode --%s value for respawn: %w", name, err)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", fmt.Errorf("encode --%s value for respawn: %w", name, err)
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

func newBackgroundSendCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "send <id> <message>",
		Short: "Send a follow-up to a detached Gokin session",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			message := strings.TrimSpace(strings.Join(args[1:], " "))
			if strings.HasPrefix(message, "/") {
				return fmt.Errorf("slash commands are not accepted by detached headless sessions; use gokin stop for cancellation")
			}
			store, err := backgroundstore.NewStore()
			if err != nil {
				return err
			}
			job, control, err := enqueueLiveBackgroundControl(store, args[0], message)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Queued input %s for background session %s.\n",
				control.ID[:8], job.ID[:8])
			return nil
		},
	}
}

func newBackgroundAttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <id>",
		Short: "Follow a detached session and send input from this terminal",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := backgroundstore.NewStore()
			if err != nil {
				return err
			}
			job, err := resolveBackgroundJob(store, args[0])
			if err != nil {
				return err
			}
			job, err = store.Reconcile(job)
			if err != nil {
				return err
			}
			if job.Terminal() {
				return fmt.Errorf("background job %s is already %s; use gokin logs %s",
					job.ID[:8], job.State, job.ID[:8])
			}
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Attached to %s. Enter a line to steer/queue it; /detach returns without stopping it.\n",
				job.ID[:8])
			attachCtx, detach := context.WithCancel(cmd.Context())
			defer detach()
			detached := make(chan struct{}, 1)
			go func() {
				scanner := bufio.NewScanner(cmd.InOrStdin())
				scanner.Buffer(make([]byte, 64<<10), 64<<10)
				for scanner.Scan() {
					message := strings.TrimSpace(scanner.Text())
					if message == "" {
						continue
					}
					if message == "/detach" || message == "/exit" {
						detached <- struct{}{}
						detach()
						return
					}
					if strings.HasPrefix(message, "/") {
						fmt.Fprintf(cmd.ErrOrStderr(), "Unsupported attach command %q; use /detach or run gokin stop in another terminal.\n", message)
						continue
					}
					if _, _, sendErr := enqueueLiveBackgroundControl(store, job.ID, message); sendErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "Failed to queue input: %v\n", sendErr)
						return
					}
				}
			}()
			err = followBackgroundLogs(attachCtx, cmd.OutOrStdout(), cmd.ErrOrStderr(), store, job)
			select {
			case <-detached:
				return nil
			default:
				return err
			}
		},
	}
}

func enqueueLiveBackgroundControl(
	store *backgroundstore.Store,
	query, message string,
) (backgroundstore.Job, backgroundstore.Control, error) {
	if store == nil {
		return backgroundstore.Job{}, backgroundstore.Control{}, fmt.Errorf("background store is not initialized")
	}
	job, err := resolveBackgroundJob(store, query)
	if err != nil {
		return backgroundstore.Job{}, backgroundstore.Control{}, err
	}
	job, err = store.Reconcile(job)
	if err != nil {
		return backgroundstore.Job{}, backgroundstore.Control{}, err
	}
	if job.Terminal() || job.State == backgroundstore.StateStopping || job.State == backgroundstore.StateFinishing {
		return backgroundstore.Job{}, backgroundstore.Control{},
			fmt.Errorf("background job %s is not accepting input in state %s", job.ID[:8], job.State)
	}
	held, err := store.WorkerLeaseHeld(job.ID)
	if err != nil {
		return backgroundstore.Job{}, backgroundstore.Control{}, err
	}
	if !held {
		return backgroundstore.Job{}, backgroundstore.Control{},
			fmt.Errorf("background job %s has no live worker", job.ID[:8])
	}
	control, err := store.EnqueueControl(job.ID, message)
	if err != nil {
		return backgroundstore.Job{}, backgroundstore.Control{}, err
	}
	return job, control, nil
}

func printTail(w io.Writer, path string, lines int) error {
	const maxTailBytes = 4 << 20
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	// Seek instead of reading the whole file: a long-running detached worker's
	// JSONL log is unbounded, and reading it just to trim to the tail would
	// make `gokin logs` allocate the entire file.
	if info, statErr := file.Stat(); statErr == nil && info.Size() > maxTailBytes {
		if _, seekErr := file.Seek(info.Size()-maxTailBytes, io.SeekStart); seekErr != nil {
			return seekErr
		}
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTailBytes))
	if err != nil {
		return err
	}
	parts := strings.Split(string(data), "\n")
	start := len(parts) - lines - 1
	if start < 0 {
		start = 0
	}
	_, err = io.WriteString(w, strings.Join(parts[start:], "\n"))
	return err
}

func followBackgroundLogs(ctx context.Context, stdout, stderr io.Writer, store *backgroundstore.Store, job backgroundstore.Job) error {
	stdoutPath, _ := store.StdoutPath(job.ID)
	stderrPath, _ := store.StderrPath(job.ID)
	var stdoutOffset, stderrOffset int64
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var err error
		stdoutOffset, err = copyLogGrowth(stdout, stdoutPath, stdoutOffset)
		if err != nil {
			return err
		}
		stderrOffset, err = copyLogGrowth(stderr, stderrPath, stderrOffset)
		if err != nil {
			return err
		}
		current, loadErr := store.Load(job.ID)
		if loadErr != nil {
			return loadErr
		}
		current, loadErr = store.Reconcile(current)
		if loadErr != nil {
			return loadErr
		}
		if current.Terminal() {
			// One final read closes the exit/write observation race.
			stdoutOffset, err = copyLogGrowth(stdout, stdoutPath, stdoutOffset)
			if err != nil {
				return err
			}
			_, err = copyLogGrowth(stderr, stderrPath, stderrOffset)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func copyLogGrowth(w io.Writer, path string, offset int64) (int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return offset, nil
	}
	if err != nil {
		return offset, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return offset, err
	}
	if !info.Mode().IsRegular() {
		return offset, fmt.Errorf("background log %q is not a regular file", path)
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	written, err := io.Copy(w, bufio.NewReader(file))
	return offset + written, err
}

// resolveBackgroundJob resolves an ID or prefix and, when it cannot, points at
// the command that lists the valid ones. This error is reached at exactly the
// moment the user does not know what to type — a stale ID, a typo, a session
// that has aged out — and the store cannot say so itself: it does not know it
// is being driven from a CLI.
func resolveBackgroundJob(store *backgroundstore.Store, query string) (backgroundstore.Job, error) {
	job, err := store.Resolve(query)
	if err != nil {
		return backgroundstore.Job{}, fmt.Errorf("%w (run `gokin agents` to list background sessions)", err)
	}
	return job, nil
}
