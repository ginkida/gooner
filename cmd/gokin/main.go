package main

import (
	"errors"
	"fmt"
	"os"

	"gokin/internal/app"
	"gokin/internal/config"
	"gokin/internal/setup"

	"github.com/spf13/cobra"
)

var (
	version  = "0.1.0"
	cfgFile  string
	model    string
	runSetup bool
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "gokin",
		Short: "AI-powered CLI assistant for code",
		Long: `Gokin is a CLI tool that uses Gemini API and GLM API to help you work with code.
It provides an interactive chat interface with tools for reading, writing,
and editing files, running commands, and more.`,
		RunE: runApp,
	}

	// Global flags
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default is $HOME/.config/gokin/config.yaml)")
	rootCmd.PersistentFlags().StringVar(&model, "model", "", "model to use (default is gemini-3-flash-preview)")
	rootCmd.PersistentFlags().BoolVar(&runSetup, "setup", false, "run the setup wizard")

	// Version command
	rootCmd.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the version number",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("gokin version %s\n", version)
		},
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runApp(cmd *cobra.Command, args []string) error {
	// Run setup wizard if requested
	if runSetup {
		if err := setup.RunSetupWizard(); err != nil {
			return err
		}
		// Continue to start the app after setup
	}

	// Load configuration
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Override model if specified
	if model != "" {
		cfg.Model.Name = model
	}

	// Validate configuration - if no API key, run setup wizard automatically
	if err := cfg.Validate(); err != nil {
		if errors.Is(err, config.ErrMissingAuth) {
			// No API key configured - run setup wizard
			if err := setup.RunSetupWizard(); err != nil {
				return err
			}
			// Reload config after setup
			cfg, err = config.Load()
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
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
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("failed to get working directory: %w", err)
	}

	// Create and run the application
	application, err := app.New(cfg, workDir)
	if err != nil {
		return fmt.Errorf("failed to create application: %w", err)
	}

	fmt.Println("\nStarting Gokin...")
	return application.Run()
}
