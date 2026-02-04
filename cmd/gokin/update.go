package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"gokin/internal/config"
	"gokin/internal/update"

	"github.com/spf13/cobra"
)

func newUpdateCmd() *cobra.Command {
	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Manage application updates",
		Long:  `Check for updates, install new versions, and manage rollbacks.`,
	}

	updateCmd.AddCommand(newUpdateCheckCmd())
	updateCmd.AddCommand(newUpdateInstallCmd())
	updateCmd.AddCommand(newUpdateRollbackCmd())
	updateCmd.AddCommand(newUpdateListBackupsCmd())

	return updateCmd
}

func newUpdateCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Check for available updates",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			updateCfg := convertConfig(&cfg.Update)
			updater, err := update.NewUpdater(updateCfg, version)
			if err != nil {
				return fmt.Errorf("failed to create updater: %w", err)
			}
			defer updater.Cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			fmt.Println("Checking for updates...")
			info, err := updater.CheckForUpdate(ctx)
			if err != nil {
				if err == update.ErrSameVersion {
					fmt.Printf("You are running the latest version (%s)\n", version)
					return nil
				}
				return fmt.Errorf("failed to check for updates: %w", err)
			}

			fmt.Printf("\nUpdate available!\n")
			fmt.Printf("  Current version: %s\n", info.CurrentVersion)
			fmt.Printf("  New version:     %s\n", info.NewVersion)
			fmt.Printf("  Published:       %s\n", info.PublishedAt.Format("2006-01-02"))
			if info.ReleaseURL != "" {
				fmt.Printf("  Release notes:   %s\n", info.ReleaseURL)
			}
			fmt.Printf("\nRun 'gokin update install' to update.\n")

			return nil
		},
	}
}

func newUpdateInstallCmd() *cobra.Command {
	var forceUpdate bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download and install the latest update",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			updateCfg := convertConfig(&cfg.Update)
			updater, err := update.NewUpdater(updateCfg, version)
			if err != nil {
				return fmt.Errorf("failed to create updater: %w", err)
			}
			defer updater.Cleanup()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()

			// Progress callback
			progress := func(p *update.UpdateProgress) {
				switch p.Status {
				case update.StatusChecking:
					fmt.Printf("\r%s...", p.Message)
				case update.StatusDownloading:
					if p.TotalBytes > 0 {
						fmt.Printf("\rDownloading: %.1f%% (%d/%d bytes)", p.Percent, p.BytesDownloaded, p.TotalBytes)
					} else {
						fmt.Printf("\rDownloading: %d bytes", p.BytesDownloaded)
					}
				case update.StatusVerifying:
					fmt.Printf("\r%s                              \n", p.Message)
				case update.StatusInstalling:
					fmt.Printf("\r%s                              \n", p.Message)
				case update.StatusComplete:
					fmt.Printf("\r%s                              \n", p.Message)
				}
			}

			info, err := updater.Update(ctx, progress)
			if err != nil {
				if err == update.ErrSameVersion && !forceUpdate {
					fmt.Printf("\nYou are already running the latest version (%s)\n", version)
					return nil
				}
				return fmt.Errorf("update failed: %w", err)
			}

			fmt.Printf("\nSuccessfully updated from %s to %s\n", info.CurrentVersion, info.NewVersion)
			fmt.Println("Please restart gokin to use the new version.")

			return nil
		},
	}

	cmd.Flags().BoolVarP(&forceUpdate, "force", "f", false, "Force update even if already up-to-date")

	return cmd
}

func newUpdateRollbackCmd() *cobra.Command {
	var backupID string

	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback to a previous version",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			updateCfg := convertConfig(&cfg.Update)
			updater, err := update.NewUpdater(updateCfg, version)
			if err != nil {
				return fmt.Errorf("failed to create updater: %w", err)
			}

			if backupID != "" {
				// Rollback to specific backup
				if err := updater.RollbackTo(backupID); err != nil {
					return fmt.Errorf("rollback failed: %w", err)
				}
				fmt.Printf("Successfully rolled back to backup %s\n", backupID)
			} else {
				// Rollback to latest backup
				if err := updater.Rollback(); err != nil {
					return fmt.Errorf("rollback failed: %w", err)
				}
				fmt.Println("Successfully rolled back to previous version")
			}

			fmt.Println("Please restart gokin to use the restored version.")
			return nil
		},
	}

	cmd.Flags().StringVar(&backupID, "backup", "", "Specific backup ID to rollback to")

	return cmd
}

func newUpdateListBackupsCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list-backups",
		Aliases: []string{"backups"},
		Short:   "List available backup versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}

			updateCfg := convertConfig(&cfg.Update)
			updater, err := update.NewUpdater(updateCfg, version)
			if err != nil {
				return fmt.Errorf("failed to create updater: %w", err)
			}

			backups, err := updater.ListBackups()
			if err != nil {
				return fmt.Errorf("failed to list backups: %w", err)
			}

			if len(backups) == 0 {
				fmt.Println("No backups available.")
				return nil
			}

			fmt.Println("Available backups:")
			fmt.Println()
			for _, b := range backups {
				fmt.Printf("  ID:       %s\n", b.ID)
				fmt.Printf("  Version:  %s\n", b.Version)
				fmt.Printf("  Created:  %s\n", b.CreatedAt.Format("2006-01-02 15:04:05"))
				fmt.Println()
			}

			fmt.Println("Use 'gokin update rollback --backup <ID>' to restore a specific backup.")

			return nil
		},
	}
}

// convertConfig converts config.UpdateConfig to update.Config.
func convertConfig(cfg *config.UpdateConfig) *update.Config {
	return &update.Config{
		Enabled:           cfg.Enabled,
		AutoCheck:         cfg.AutoCheck,
		CheckInterval:     cfg.CheckInterval,
		AutoDownload:      cfg.AutoDownload,
		IncludePrerelease: cfg.IncludePrerelease,
		Channel:           update.Channel(cfg.Channel),
		GitHubRepo:        cfg.GitHubRepo,
		MaxBackups:        cfg.MaxBackups,
		VerifyChecksum:    cfg.VerifyChecksum,
		NotifyOnly:        cfg.NotifyOnly,
		Timeout:           30 * time.Second,
	}
}

// CheckForUpdateOnStartup checks for updates on startup and notifies the user.
func CheckForUpdateOnStartup(cfg *config.Config, app update.AppInterface) {
	if !cfg.Update.Enabled || !cfg.Update.AutoCheck {
		return
	}

	updateCfg := convertConfig(&cfg.Update)
	updater, err := update.NewUpdater(updateCfg, version)
	if err != nil {
		return // Silently fail
	}
	defer updater.Cleanup()

	// Don't check if we checked recently
	if !updater.ShouldAutoCheck() {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	info, err := updater.CheckForUpdateIfDue(ctx)
	if err != nil {
		return // Silently fail
	}

	// Notify user via TUI if available
	if app != nil {
		msg := fmt.Sprintf("📦 **Update available: %s → %s**\n\n"+
			"• Type `/update` to check details\n"+
			"• Type `/update install` to install now\n"+
			"• Or exit and run: `gokin update install`",
			info.CurrentVersion, info.NewVersion)

		// Delay slightly to ensure UI is ready
		time.Sleep(2 * time.Second)
		app.AddSystemMessage(msg)
	} else {
		// Fallback to stderr if not in TUI mode
		fmt.Fprintf(os.Stderr, "\n📦 Update available: %s → %s\n", info.CurrentVersion, info.NewVersion)
		fmt.Fprintf(os.Stderr, "   Run 'gokin update install' to update.\n\n")
	}
}
