package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appcontext "gokin/internal/context"
	"gokin/internal/undo"
)

// MaxUndoSteps caps how many changes /undo N will revert in one call. Prevents
// accidental massive rewinds ("/undo 999" when user meant "/undo list").
const MaxUndoSteps = 20

// UndoCommand reverts the last file change, or multiple changes with /undo N.
type UndoCommand struct{}

func (c *UndoCommand) Name() string        { return "undo" }
func (c *UndoCommand) Description() string { return "Undo last file change(s)" }
func (c *UndoCommand) Usage() string {
	return `/undo           - Undo last file change
/undo all       - Undo every change the last request made, atomically
/undo N         - Undo last N changes (max 20)
/undo list      - Show recent undoable changes`
}
func (c *UndoCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "undo",
		Priority: 70,
		HasArgs:  true,
		ArgHint:  "[N|all|list]",
	}
}

// remainingInRequest counts changes still on the stack that came from the same
// request as the one just reverted. The message processor stamps a per-request
// group on every change it records; until now nothing read it back, so a user
// who asked for a five-file change had no way to learn that "/undo" had put
// only one of them back.
func remainingInRequest(mgr *undo.Manager, groupID string) int {
	if groupID == "" {
		return 0
	}
	n := 0
	for _, change := range mgr.List() {
		if change.GroupID == groupID {
			n++
		}
	}
	return n
}

func safeChangeSummary(summary string) string {
	if clean := singleLineDisplayText(summary, 160); clean != "" {
		return clean
	}
	return "file change"
}

func safeUndoError(err error) string {
	if err == nil {
		return "unknown error"
	}
	if clean := singleLineDisplayText(err.Error(), 200); clean != "" {
		return clean
	}
	return "unknown error"
}

func (c *UndoCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	mgr := app.GetUndoManager()
	if mgr == nil {
		return "Undo manager not available.", nil
	}

	// /undo list — preview recent changes without reverting anything.
	if len(args) > 0 && strings.EqualFold(args[0], "list") {
		recent := mgr.ListRecent(10)
		if len(recent) == 0 {
			return "No changes to undo.", nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Recent undoable changes (most recent first, total %d):\n", mgr.Count())
		for i, change := range recent {
			fmt.Fprintf(&sb, "  %2d. %s\n", i+1, safeChangeSummary(change.Summary()))
		}
		sb.WriteString("\n1 is the next change /undo will revert. Use /undo N to revert through item N.")
		return sb.String(), nil
	}

	// /undo all — revert everything the last request changed, as ONE
	// transaction. The per-request group has been stamped on every change since
	// the message processor started recording them, and the group path
	// preflights the whole set before its first write and rolls back on
	// failure — neither of which the /undo N loop below can do.
	if len(args) > 0 && (strings.EqualFold(args[0], "all") || strings.EqualFold(args[0], "request")) {
		changes, err := mgr.UndoLastGroup()
		if err != nil {
			return fmt.Sprintf("Undo: %s", safeUndoError(err)), nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Undone %d change(s) from the last request:\n", len(changes))
		for i, change := range changes {
			fmt.Fprintf(&sb, "  %d. %s\n", i+1, safeChangeSummary(change.Summary()))
		}
		if len(changes) == 1 {
			sb.WriteString("Redo this change: /redo")
		} else {
			sb.WriteString("Redo the whole request: /redo all")
		}
		return sb.String(), nil
	}

	// /undo N — multi-step rewind. Stops at the first error and reports what
	// was actually reverted so the user isn't left guessing the stack state.
	steps := 1
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return fmt.Sprintf("Invalid argument %q. Use /undo [N|all|list].", args[0]), nil
		}
		if n > MaxUndoSteps {
			return fmt.Sprintf("Max %d steps per /undo. Run again if you need more.", MaxUndoSteps), nil
		}
		steps = n
	}

	if steps == 1 {
		change, err := mgr.Undo()
		if err != nil {
			return fmt.Sprintf("Undo: %s", safeUndoError(err)), nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Undone: %s\n", safeChangeSummary(change.Summary()))
		if n := remainingInRequest(mgr, change.GroupID); n > 0 {
			fmt.Fprintf(&sb, "%d more change(s) from that same request remain — /undo all reverts them together.\n", n)
		}
		sb.WriteString("Redo this change: /redo")
		return sb.String(), nil
	}

	var reverted []string
	var stopErr error
	for range steps {
		change, err := mgr.Undo()
		if err != nil {
			if len(reverted) == 0 {
				return fmt.Sprintf("Undo: %s", safeUndoError(err)), nil
			}
			stopErr = err
			break
		}
		reverted = append(reverted, safeChangeSummary(change.Summary()))
	}
	var sb strings.Builder
	if stopErr != nil {
		fmt.Fprintf(&sb, "Undone %d of %d requested change(s); stopped: %s\n", len(reverted), steps, safeUndoError(stopErr))
	} else {
		fmt.Fprintf(&sb, "Undone %d change(s):\n", len(reverted))
	}
	for i, s := range reverted {
		fmt.Fprintf(&sb, "  %d. %s\n", i+1, s)
	}
	fmt.Fprintf(&sb, "Redo these changes: /redo %d", len(reverted))
	return sb.String(), nil
}

// RedoCommand re-applies previously undone changes.
type RedoCommand struct{}

func (c *RedoCommand) Name() string        { return "redo" }
func (c *RedoCommand) Description() string { return "Redo last undone change(s)" }
func (c *RedoCommand) Usage() string {
	return `/redo           - Redo last undone change
/redo all       - Redo the whole request that /undo all reverted
/redo N         - Redo last N undone changes (max 20)`
}
func (c *RedoCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "redo",
		Priority: 71,
		HasArgs:  true,
		ArgHint:  "[N|all]",
	}
}

func (c *RedoCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	mgr := app.GetUndoManager()
	if mgr == nil {
		return "Undo manager not available.", nil
	}

	// /redo all — the mirror of /undo all. Re-applies the whole request as one
	// transaction, preflighted before its first write and rolled back if a
	// later change fails, rather than as N independent steps the user has to
	// count out themselves.
	if len(args) > 0 && (strings.EqualFold(args[0], "all") || strings.EqualFold(args[0], "request")) {
		changes, err := mgr.RedoLastGroup()
		if err != nil {
			return fmt.Sprintf("Redo: %s", safeUndoError(err)), nil
		}
		var sb strings.Builder
		fmt.Fprintf(&sb, "Redone %d change(s) from the last request:\n", len(changes))
		for i, change := range changes {
			fmt.Fprintf(&sb, "  %d. %s\n", i+1, safeChangeSummary(change.Summary()))
		}
		if len(changes) == 1 {
			sb.WriteString("Undo this change: /undo")
		} else {
			sb.WriteString("Undo the whole request again: /undo all")
		}
		return sb.String(), nil
	}

	steps := 1
	if len(args) > 0 {
		n, err := strconv.Atoi(args[0])
		if err != nil || n < 1 {
			return fmt.Sprintf("Invalid argument %q. Use /redo [N|all].", args[0]), nil
		}
		if n > MaxUndoSteps {
			return fmt.Sprintf("Max %d steps per /redo. Run again if you need more.", MaxUndoSteps), nil
		}
		steps = n
	}

	if steps == 1 {
		change, err := mgr.Redo()
		if err != nil {
			return fmt.Sprintf("Redo: %s", safeUndoError(err)), nil
		}
		return fmt.Sprintf("Redone: %s\nUndo this change again: /undo", safeChangeSummary(change.Summary())), nil
	}

	var applied []string
	var stopErr error
	for range steps {
		change, err := mgr.Redo()
		if err != nil {
			if len(applied) == 0 {
				return fmt.Sprintf("Redo: %s", safeUndoError(err)), nil
			}
			stopErr = err
			break
		}
		applied = append(applied, safeChangeSummary(change.Summary()))
	}
	var sb strings.Builder
	if stopErr != nil {
		fmt.Fprintf(&sb, "Redone %d of %d requested change(s); stopped: %s\n", len(applied), steps, safeUndoError(stopErr))
	} else {
		fmt.Fprintf(&sb, "Redone %d change(s):\n", len(applied))
	}
	for i, s := range applied {
		fmt.Fprintf(&sb, "  %d. %s\n", i+1, s)
	}
	fmt.Fprintf(&sb, "Undo these changes again: /undo %d", len(applied))
	return sb.String(), nil
}

// CostCommand shows token usage (alias for /stats).
type CostCommand struct{}

func (c *CostCommand) Name() string        { return "cost" }
func (c *CostCommand) Description() string { return "Show token usage and cost" }
func (c *CostCommand) Usage() string       { return "/cost" }
func (c *CostCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySession,
		Icon:     "stats",
		Priority: 61,
	}
}

func (c *CostCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	stats := app.GetTokenStats()

	var costStr string
	if stats.CostTracked {
		costStr = fmt.Sprintf("\n  Cost:   %s", appcontext.FormatCost(stats.EstimatedCost))
	} else if cm := app.GetContextManager(); cm != nil {
		tc := cm.GetTokenCounter()
		if tc != nil {
			cost := tc.CalculateCostWithCache(stats.InputTokens, stats.OutputTokens, stats.CacheReadInputTokens)
			costStr = fmt.Sprintf("\n  Cost:   %s", appcontext.FormatCost(cost))
		}
	}

	modelStr := ""
	if ms := app.GetModelSetter(); ms != nil {
		if m := ms.GetModel(); m != "" {
			modelStr = fmt.Sprintf("\n  Model:  %s", m)
		}
	}

	return fmt.Sprintf("Token usage:\n  Input:  %s\n  Output: %s\n  Total:  %s%s%s",
		formatTokens(stats.InputTokens), formatTokens(stats.OutputTokens),
		formatTokens(stats.TotalTokens), costStr, modelStr), nil
}

// formatTokens renders a token count with the same K/M shape the status
// bar uses (internal/ui/tui_tokens.go formatTokens) — uppercase K so the
// number reads as "kilo" in the engineering sense, not the lowercase
// "kilo" of physics units. Keeps /cost output visually aligned with the
// always-on status-bar token segment.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
