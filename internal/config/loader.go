package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Load loads configuration from file and environment variables.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	// Try to load from config file
	configPath := getConfigPath()
	if configPath != "" {
		if err := loadFromFile(cfg, configPath); err != nil {
			// Config file is optional, don't fail if it doesn't exist
			if !os.IsNotExist(err) {
				return nil, err
			}
		}
	}

	// Override with environment variables
	loadFromEnv(cfg)

	return cfg, nil
}

// getConfigPath returns the path to the config file.
func getConfigPath() string {
	// Check XDG_CONFIG_HOME first
	if xdgConfig := os.Getenv("XDG_CONFIG_HOME"); xdgConfig != "" {
		return filepath.Join(xdgConfig, "gokin", "config.yaml")
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	// For macOS, favor Library/Application Support/gokin if it exists or if we're on darwin
	if runtime.GOOS == "darwin" {
		appSupport := filepath.Join(homeDir, "Library", "Application Support", "gokin", "config.yaml")
		if _, err := os.Stat(appSupport); err == nil {
			return appSupport
		}
		// Fall back to .config if it already exists there
		dotConfig := filepath.Join(homeDir, ".config", "gokin", "config.yaml")
		if _, err := os.Stat(dotConfig); err == nil {
			return dotConfig
		}
		// Default to App Support for new installs on macOS
		return appSupport
	}

	// Default for other Unix-like systems
	return filepath.Join(homeDir, ".config", "gokin", "config.yaml")
}

// loadFromFile loads configuration from a YAML file.
func loadFromFile(cfg *Config, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Expand environment variables in the config file
	expanded := os.ExpandEnv(string(data))

	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", path, err)
	}

	return nil
}

// loadFromEnv loads configuration from environment variables.
func loadFromEnv(cfg *Config) {
	// API key from environment (check multiple sources)
	// Priority: GOKIN_API_KEY > GLM_API_KEY > GEMINI_API_KEY
	if apiKey := os.Getenv("GOKIN_API_KEY"); apiKey != "" {
		cfg.API.APIKey = apiKey
	} else if apiKey := os.Getenv("GLM_API_KEY"); apiKey != "" {
		cfg.API.APIKey = apiKey
		if cfg.API.Backend == "" {
			cfg.API.Backend = "glm"
		}
	} else if apiKey := os.Getenv("GEMINI_API_KEY"); apiKey != "" {
		cfg.API.APIKey = apiKey
	}

	if model := os.Getenv("GOKIN_MODEL"); model != "" {
		cfg.Model.Name = model
	}

	if backend := os.Getenv("GOKIN_BACKEND"); backend != "" {
		cfg.API.Backend = backend
	}
}

// Validate validates the configuration.
func (c *Config) Validate() error {
	if c.API.APIKey == "" {
		return ErrMissingAuth
	}
	return nil
}

// Error types for configuration validation.
type ConfigError string

func (e ConfigError) Error() string {
	return string(e)
}

const (
	ErrMissingAuth ConfigError = "missing authentication: set GEMINI_API_KEY or GLM_API_KEY environment variable, or use /login <api_key>"
)

// GetConfigPath returns the path to the config file (exported for external use).
func GetConfigPath() string {
	return getConfigPath()
}

// Save saves the configuration to the config file.
func (c *Config) Save() error {
	configPath := getConfigPath()
	if configPath == "" {
		return fmt.Errorf("could not determine config path")
	}

	// Ensure config directory exists (0700 for security - only owner can access)
	configDir := filepath.Dir(configPath)
	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	// Marshal config to YAML with proper ordering
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	// Write to file atomically (write to temp file then rename)
	// Use 0600 permissions for security - config may contain API keys
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	// Rename temp file to actual config file (atomic on POSIX systems)
	if err := os.Rename(tmpPath, configPath); err != nil {
		// If rename fails, try direct write (Windows filesystem)
		if err := os.WriteFile(configPath, data, 0600); err != nil {
			return fmt.Errorf("failed to write config file: %w", err)
		}
	}

	return nil
}

// IsWorkDirAllowed checks if a working directory is in the allowed list.
func (c *Config) IsWorkDirAllowed(workDir string) bool {
	// Clean and resolve the path
	absWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return false
	}
	absWorkDir = filepath.Clean(absWorkDir)

	for _, dir := range c.Tools.AllowedDirs {
		absDir, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		absDir = filepath.Clean(absDir)

		// Check if workDir is within this allowed dir
		if absWorkDir == absDir || strings.HasPrefix(absWorkDir, absDir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// AddAllowedDir adds a directory to the allowed list if not already present.
func (c *Config) AddAllowedDir(dir string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	absDir = filepath.Clean(absDir)

	// Check if already in list
	for _, existing := range c.Tools.AllowedDirs {
		absExisting, err := filepath.Abs(existing)
		if err != nil {
			continue
		}
		if filepath.Clean(absExisting) == absDir {
			return false // Already exists
		}
	}

	c.Tools.AllowedDirs = append(c.Tools.AllowedDirs, absDir)
	return true
}
