package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gokin/internal/config"
	appcontext "gokin/internal/context"
	"gokin/internal/format"
	"gokin/internal/mcp"
)

// StatsCommand shows detailed session statistics.
type StatsCommand struct{}

func (c *StatsCommand) Name() string        { return "stats" }
func (c *StatsCommand) Description() string { return "Show detailed session statistics" }
func (c *StatsCommand) Usage() string       { return "/stats" }
func (c *StatsCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category:    CategorySession,
		Icon:        "stats",
		Priority:    60,
		RequiresAPI: true,
	}
}

func (c *StatsCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	var sb strings.Builder

	// Get token stats
	tokenStats := app.GetTokenStats()

	// Get config
	cfg := app.GetConfig()

	// Get project info
	projectInfo := app.GetProjectInfo()

	// Header — clean rule, no emoji.
	sb.WriteString("Session Statistics\n")
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n\n")

	// Token Usage. InputTokens is the full prompt-side total; the cache fields
	// are subsets/partitions reported separately for transparency.
	sb.WriteString("Tokens\n")
	fmt.Fprintf(&sb, "  Prompt Input:     %s\n", formatNumber(int64(tokenStats.InputTokens)))
	fmt.Fprintf(&sb, "  Output Tokens:    %s\n", formatNumber(int64(tokenStats.OutputTokens)))
	if tokenStats.CacheReadInputTokens > 0 {
		fmt.Fprintf(&sb, "  Cached Input:     %s\n", formatNumber(int64(tokenStats.CacheReadInputTokens)))
	}
	if tokenStats.CacheCreationInputTokens > 0 {
		fmt.Fprintf(&sb, "  Cache Created:    %s\n", formatNumber(int64(tokenStats.CacheCreationInputTokens)))
	}
	if tokenStats.InputTokens > 0 {
		uncached := max(tokenStats.InputTokens-tokenStats.CacheReadInputTokens, 0)
		fmt.Fprintf(&sb, "  Uncached Input:   %s\n", formatNumber(int64(uncached)))
		fmt.Fprintf(&sb, "  Prompt Cache Hit: %.1f%%\n",
			100*float64(tokenStats.CacheReadInputTokens)/float64(tokenStats.InputTokens))
	}
	if tokenStats.PromptCacheBreaks > 0 {
		fmt.Fprintf(&sb, "  Cache Breaks:     %d", tokenStats.PromptCacheBreaks)
		if tokenStats.LastPromptCacheBreakReason != "" {
			fmt.Fprintf(&sb, " (%s)", tokenStats.LastPromptCacheBreakReason)
		}
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "  Total Tokens:     %s\n", formatNumber(int64(tokenStats.TotalTokens)))

	// Calculate cost using per-model pricing from TokenCounter
	totalCost := tokenStats.EstimatedCost
	contextManager := app.GetContextManager()
	if !tokenStats.CostTracked && contextManager != nil {
		tc := contextManager.GetTokenCounter()
		if tc != nil {
			totalCost = tc.CalculateCostWithCache(tokenStats.InputTokens, tokenStats.OutputTokens, tokenStats.CacheReadInputTokens)
		}
	}
	fmt.Fprintf(&sb, "  Est. Cost:       %s\n\n", appcontext.FormatCost(totalCost))

	// Model Info
	sb.WriteString("Model\n")
	fmt.Fprintf(&sb, "  Name:            %s\n", cfg.Model.Name)
	fmt.Fprintf(&sb, "  Temperature:     %.1f\n", cfg.Model.Temperature)
	fmt.Fprintf(&sb, "  Max Tokens:      %s\n\n", formatNumber(int64(cfg.Model.MaxOutputTokens)))

	// Context Info
	sb.WriteString("Context\n")
	if contextManager == nil {
		contextManager = app.GetContextManager()
	}
	if contextManager != nil {
		metrics := contextManager.GetMetrics()
		summary := metrics.GetSummary()

		fmt.Fprintf(&sb, "  Requests:        %d\n", summary.Requests)
		fmt.Fprintf(&sb, "  Optimizations:   %d\n", summary.Optimizations)
		fmt.Fprintf(&sb, "  Summaries:       %d\n", summary.Summaries)
		fmt.Fprintf(&sb, "  Tokens Processed: %s\n", formatNumber(summary.TokensProcessed))
		fmt.Fprintf(&sb, "  Tokens Saved:     %s\n", formatNumber(summary.TokensSaved))
		fmt.Fprintf(&sb, "  Summary Cache Hit: %.1f%%\n\n", summary.CacheHitRate*100)
	} else {
		sb.WriteString("  (context manager not available)\n\n")
	}

	// Session Info
	sb.WriteString("Session\n")
	if session := app.GetSession(); session != nil {
		history := session.GetHistory()
		fmt.Fprintf(&sb, "  Messages:        %d\n", len(history))
	}

	// Project Info
	sb.WriteString("\nProject\n")
	if projectInfo != nil {
		fmt.Fprintf(&sb, "  Name:            %s\n", projectInfo.Name)
		fmt.Fprintf(&sb, "  Type:            %s\n", projectInfo.Type)
		sb.WriteString("\n")
	} else {
		sb.WriteString("  (no project info available)\n\n")
	}

	// Session Duration
	sessionStartTime := ctx.Value("session_start")
	if sessionStartTime != nil {
		if startTime, ok := sessionStartTime.(time.Time); ok {
			duration := time.Since(startTime)
			sb.WriteString("Duration\n")
			fmt.Fprintf(&sb, "  Session Length:  %s\n\n", format.Duration(duration))
		}
	}

	// Performance stats (phase latency + tool call breakdown)
	if perf := app.GetPerformanceStats(); perf != "" {
		sb.WriteString(perf)
	}

	// Lifetime tool usage — the only figure here that survives /clear and
	// restarts, and therefore the only one that can answer whether a tool has
	// ever been reached for at all.
	if usage := formatLifetimeToolUsage(app.GetLifetimeToolUsage()); usage != "" {
		sb.WriteString(usage)
	}

	// The audit log runs on every tool call and has no reader inside the app,
	// so the only way it is useful is if the user can find the files.
	if audit := formatAuditLogLocation(cfg); audit != "" {
		sb.WriteString(audit)
	}

	// MCP section — only shown when at least one server is configured so we
	// don't clutter /stats for users who never opted in.
	if mgr := app.GetMCPManager(); mgr != nil {
		if mcpSection := formatMCPStatsSection(mgr); mcpSection != "" {
			sb.WriteString(mcpSection)
		}
	}

	// Footer
	sb.WriteString(strings.Repeat("─", 50))
	sb.WriteString("\n")
	sb.WriteString("Tip: /cost shows real-time token usage")

	return sb.String(), nil
}

// formatMCPStatsSection summarizes MCP server state for /stats. Returns an
// empty string when no servers are configured so callers can skip the header.
func formatMCPStatsSection(mgr *mcp.Manager) string {
	statuses := mgr.GetServerStatus()
	if len(statuses) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("MCP\n")
	connected := 0
	tools := 0
	unhealthy := 0
	for _, s := range statuses {
		if s.Connected {
			connected++
			if !s.Healthy {
				unhealthy++
			}
		}
		tools += s.ToolCount
	}
	fmt.Fprintf(&sb, "  Servers:         %d total, %d connected\n", len(statuses), connected)
	fmt.Fprintf(&sb, "  Tools exposed:   %d\n", tools)
	if unhealthy > 0 {
		fmt.Fprintf(&sb, "  Unhealthy:       %d (run /mcp status for detail)\n", unhealthy)
	}
	sb.WriteByte('\n')
	return sb.String()
}
func formatNumber(n int64) string {
	// Strip the sign before digit-grouping, or the '-' gets counted as a
	// digit and a comma can land right after it (e.g. -123456 -> "-,123,456"
	// instead of "-123,456") whenever the digit count is a multiple of 3.
	neg := n < 0
	in := fmt.Sprintf("%d", n)
	if neg {
		in = in[1:]
	}
	out := make([]byte, 0, len(in)+(len(in)/3))

	i := len(in)
	j := 0
	for i > 0 {
		if j == 3 {
			out = append([]byte{','}, out...)
			j = 0
		}
		i--
		out = append([]byte{in[i]}, out...)
		j++
	}

	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// formatLifetimeToolUsage renders the persisted invocation counts. It reports
// what was measured and stops there: a never-invoked tool may be genuinely
// dead or merely rare, and the difference is a judgement the reader makes with
// context this command does not have.
func formatLifetimeToolUsage(usage LifetimeToolUsage) string {
	// With nothing recorded the ledger has no opinion, and saying so out loud
	// would be worse than silence: NeverUsed is the whole registry on a fresh
	// install, so the section would report read, write and bash — the tools
	// being used at that very moment — as never invoked. "No measurements yet"
	// and "measured, and dead" must never render the same.
	if usage.Total == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Tool Usage (lifetime, all sessions)\n")
	fmt.Fprintf(&sb, "  Invocations:     %s across %d tool(s)\n",
		formatNumber(usage.Total), len(usage.Counts))

	type entry struct {
		name  string
		count int64
	}
	ranked := make([]entry, 0, len(usage.Counts))
	for name, count := range usage.Counts {
		ranked = append(ranked, entry{name, count})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count > ranked[j].count
		}
		return ranked[i].name < ranked[j].name
	})
	for i, e := range ranked {
		if i >= lifetimeUsageTopN {
			break
		}
		fmt.Fprintf(&sb, "    %-18s %s\n", e.name, formatNumber(e.count))
	}

	if len(usage.NeverUsed) > 0 {
		shown := usage.NeverUsed
		suffix := ""
		if len(shown) > lifetimeUnusedListMax {
			shown = shown[:lifetimeUnusedListMax]
			suffix = fmt.Sprintf(", +%d more", len(usage.NeverUsed)-lifetimeUnusedListMax)
		}
		fmt.Fprintf(&sb, "  Never invoked:   %d — %s%s\n",
			len(usage.NeverUsed), strings.Join(shown, ", "), suffix)
	}
	sb.WriteByte('\n')
	return sb.String()
}

const (
	lifetimeUsageTopN     = 5
	lifetimeUnusedListMax = 8
)

// formatAuditLogLocation names a subsystem that runs on every tool call and
// cannot be reached from inside the app: the audit log has no viewer here — no
// command lists it, and its query API (GetRecent/GetSessions/Export) has never
// had a production caller. The files ARE the interface, and that only works if
// the user knows they exist. Reports what is on disk and stops there, the same
// way the tool-usage section does.
func formatAuditLogLocation(cfg *config.Config) string {
	if cfg == nil || !cfg.Audit.Enabled {
		return ""
	}
	configDir, err := appcontext.GetConfigDir()
	if err != nil {
		return ""
	}
	auditDir := filepath.Join(configDir, "audit")
	entries, err := os.ReadDir(auditDir)
	if err != nil {
		// Nothing has been written yet, which is not worth a line.
		return ""
	}
	sessions := 0
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
			sessions++
		}
	}
	if sessions == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("Audit Log (tool calls, all sessions)\n")
	fmt.Fprintf(&sb, "  Location:        %s\n", auditDir)
	fmt.Fprintf(&sb, "  Session files:   %d (kept %d days)\n", sessions, cfg.Audit.RetentionDays)
	sb.WriteString("  Not readable from inside gokin — the files are JSON.\n\n")
	return sb.String()
}
