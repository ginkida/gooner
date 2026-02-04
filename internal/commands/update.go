package commands

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gokin/internal/update"
)

// UpdateCommand allows checking for and installing updates within the TUI.
type UpdateCommand struct{}

func (c *UpdateCommand) Name() string        { return "update" }
func (c *UpdateCommand) Description() string { return "Check for and install application updates" }
func (c *UpdateCommand) Usage() string {
	return `/update          - Check for available updates
/update install  - Download and install the latest version`
}

func (c *UpdateCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategorySettings,
		Icon:     "download",
		Priority: 5,
		HasArgs:  true,
		ArgHint:  "[install]",
	}
}

func (c *UpdateCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	cfg := app.GetConfig()
	if cfg == nil {
		return "Configuration not available.", nil
	}

	currentVersion := app.GetVersion()
	if currentVersion == "" {
		currentVersion = "0.1.0" // fallback
	}

	updateCfg := &update.Config{
		Enabled:           cfg.Update.Enabled,
		AutoCheck:         cfg.Update.AutoCheck,
		CheckInterval:     cfg.Update.CheckInterval,
		AutoDownload:      cfg.Update.AutoDownload,
		IncludePrerelease: cfg.Update.IncludePrerelease,
		Channel:           update.Channel(cfg.Update.Channel),
		GitHubRepo:        cfg.Update.GitHubRepo,
		MaxBackups:        cfg.Update.MaxBackups,
		VerifyChecksum:    cfg.Update.VerifyChecksum,
		NotifyOnly:        cfg.Update.NotifyOnly,
		Timeout:           30 * time.Second,
	}

	updater, err := update.NewUpdater(updateCfg, currentVersion)
	if err != nil {
		return fmt.Sprintf("Failed to initialize updater: %v", err), nil
	}
	defer updater.Cleanup()

	// Determine action
	action := "check"
	if len(args) > 0 {
		action = strings.ToLower(args[0])
	}

	switch action {
	case "install":
		return c.installUpdate(ctx, updater, currentVersion)
	default:
		return c.checkForUpdate(ctx, updater, currentVersion)
	}
}

func (c *UpdateCommand) checkForUpdate(ctx context.Context, updater *update.Updater, currentVersion string) (string, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	info, err := updater.CheckForUpdate(checkCtx)
	if err != nil {
		if err == update.ErrSameVersion {
			return fmt.Sprintf("✓ You are running the latest version (%s).", currentVersion), nil
		}
		if err == update.ErrNoUpdate {
			return "Updates are currently disabled in configuration.", nil
		}
		return fmt.Sprintf("Failed to check for updates: %v", err), nil
	}

	var sb strings.Builder
	sb.WriteString("📦 **Update available!**\n\n")
	sb.WriteString(fmt.Sprintf("Current version: `%s`\n", info.CurrentVersion))
	sb.WriteString(fmt.Sprintf("New version:     `%s`\n", info.NewVersion))
	sb.WriteString(fmt.Sprintf("Published:       %s\n", info.PublishedAt.Format("2006-01-02")))

	if info.ReleaseURL != "" {
		sb.WriteString(fmt.Sprintf("\n[View release notes](%s)\n", info.ReleaseURL))
	}

	sb.WriteString("\n**To update:**\n")
	sb.WriteString("• Run `/update install` to install now\n")
	sb.WriteString("• Or exit and run: `gokin update install`")

	return sb.String(), nil
}

func (c *UpdateCommand) installUpdate(ctx context.Context, updater *update.Updater, currentVersion string) (string, error) {
	var sb strings.Builder
	sb.WriteString("📦 **Installing update...**\n\n")

	// Use longer timeout for installation
	installCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	var lastMessage string
	progress := func(p *update.UpdateProgress) {
		lastMessage = p.Message
	}

	info, err := updater.Update(installCtx, progress)
	if err != nil {
		if err == update.ErrSameVersion {
			return fmt.Sprintf("✓ You are already running the latest version (%s).", currentVersion), nil
		}
		return fmt.Sprintf("Update failed: %v\n\nLast status: %s", err, lastMessage), nil
	}

	sb.WriteString("✓ **Update successful!**\n\n")
	sb.WriteString(fmt.Sprintf("Previous version: `%s`\n", info.CurrentVersion))
	sb.WriteString(fmt.Sprintf("New version:      `%s`\n", info.NewVersion))
	sb.WriteString("\n⚠️ **Please restart gokin to use the new version.**")

	return sb.String(), nil
}
