package context

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// GetConfigDir returns the configuration directory for the application.
// It follows XDG Base Directory Specification on Linux/macOS.
func GetConfigDir() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "gokin"), nil
}

// ProjectType represents the detected project type.
type ProjectType string

const (
	ProjectTypeGo      ProjectType = "go"
	ProjectTypeNode    ProjectType = "node"
	ProjectTypeRust    ProjectType = "rust"
	ProjectTypePython  ProjectType = "python"
	ProjectTypeJava    ProjectType = "java"
	ProjectTypeRuby    ProjectType = "ruby"
	ProjectTypePHP     ProjectType = "php"
	ProjectTypeUnknown ProjectType = "unknown"
)

// ProjectInfo contains detected project information.
type ProjectInfo struct {
	Type           ProjectType
	Name           string
	RootDir        string
	Dependencies   []string
	PackageManager string
	MainFiles      []string
	TestFramework  string
	BuildTool      string
}

// projectMarkers maps file patterns to project types.
var projectMarkers = map[ProjectType][]string{
	ProjectTypeGo:     {"go.mod", "go.sum"},
	ProjectTypeNode:   {"package.json", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb"},
	ProjectTypeRust:   {"Cargo.toml", "Cargo.lock"},
	ProjectTypePython: {"pyproject.toml", "requirements.txt", "setup.py", "Pipfile"},
	ProjectTypeJava:   {"pom.xml", "build.gradle", "build.gradle.kts"},
	ProjectTypeRuby:   {"Gemfile", "Rakefile"},
	ProjectTypePHP:    {"composer.json", "composer.lock"},
}

// DetectProject detects project type and information from the working directory.
func DetectProject(workDir string) *ProjectInfo {
	info := &ProjectInfo{
		Type:    ProjectTypeUnknown,
		RootDir: workDir,
	}

	// Check each project type
	for projectType, markers := range projectMarkers {
		for _, marker := range markers {
			markerPath := filepath.Join(workDir, marker)
			if _, err := os.Stat(markerPath); err == nil {
				info.Type = projectType
				info.extractProjectDetails(workDir, marker)
				return info
			}
		}
	}

	return info
}

// extractProjectDetails extracts additional project details based on type.
func (p *ProjectInfo) extractProjectDetails(workDir, markerFile string) {
	switch p.Type {
	case ProjectTypeGo:
		p.extractGoDetails(workDir)
	case ProjectTypeNode:
		p.extractNodeDetails(workDir)
	case ProjectTypeRust:
		p.extractRustDetails(workDir)
	case ProjectTypePython:
		p.extractPythonDetails(workDir)
	}
}

// extractGoDetails extracts Go project details.
func (p *ProjectInfo) extractGoDetails(workDir string) {
	p.BuildTool = "go"
	p.TestFramework = "go test"

	// Read go.mod for module name
	goModPath := filepath.Join(workDir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			p.Name = strings.TrimPrefix(line, "module ")
			break
		}
	}

	// Find main files
	p.MainFiles = findFiles(workDir, "main.go")
}

// extractNodeDetails extracts Node.js project details.
func (p *ProjectInfo) extractNodeDetails(workDir string) {
	// Detect package manager
	if _, err := os.Stat(filepath.Join(workDir, "bun.lockb")); err == nil {
		p.PackageManager = "bun"
	} else if _, err := os.Stat(filepath.Join(workDir, "pnpm-lock.yaml")); err == nil {
		p.PackageManager = "pnpm"
	} else if _, err := os.Stat(filepath.Join(workDir, "yarn.lock")); err == nil {
		p.PackageManager = "yarn"
	} else {
		p.PackageManager = "npm"
	}

	// Read package.json
	pkgPath := filepath.Join(workDir, "package.json")
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		return
	}

	var pkg struct {
		Name         string            `json:"name"`
		Scripts      map[string]string `json:"scripts"`
		Dependencies map[string]string `json:"dependencies"`
	}

	if err := json.Unmarshal(data, &pkg); err != nil {
		return
	}

	p.Name = pkg.Name

	// Detect test framework
	if _, ok := pkg.Scripts["test"]; ok {
		p.TestFramework = p.PackageManager + " test"
	}

	// Detect build tool from scripts
	if _, ok := pkg.Scripts["build"]; ok {
		p.BuildTool = p.PackageManager + " run build"
	}

	// Extract key dependencies
	for dep := range pkg.Dependencies {
		if isKeyDependency(dep) {
			p.Dependencies = append(p.Dependencies, dep)
		}
	}
}

// extractRustDetails extracts Rust project details.
func (p *ProjectInfo) extractRustDetails(workDir string) {
	p.BuildTool = "cargo"
	p.TestFramework = "cargo test"

	// Basic Cargo.toml parsing
	cargoPath := filepath.Join(workDir, "Cargo.toml")
	data, err := os.ReadFile(cargoPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	inPackage := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "[package]" {
			inPackage = true
			continue
		}
		if strings.HasPrefix(line, "[") {
			inPackage = false
		}
		if inPackage && strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				p.Name = strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
		}
	}
}

// extractPythonDetails extracts Python project details.
func (p *ProjectInfo) extractPythonDetails(workDir string) {
	// Detect package manager
	if _, err := os.Stat(filepath.Join(workDir, "poetry.lock")); err == nil {
		p.PackageManager = "poetry"
	} else if _, err := os.Stat(filepath.Join(workDir, "Pipfile")); err == nil {
		p.PackageManager = "pipenv"
	} else if _, err := os.Stat(filepath.Join(workDir, "uv.lock")); err == nil {
		p.PackageManager = "uv"
	} else {
		p.PackageManager = "pip"
	}

	p.TestFramework = "pytest"
	p.BuildTool = p.PackageManager

	// Try to get project name from pyproject.toml
	pyprojectPath := filepath.Join(workDir, "pyproject.toml")
	data, err := os.ReadFile(pyprojectPath)
	if err != nil {
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				p.Name = strings.Trim(strings.TrimSpace(parts[1]), "\"")
			}
			break
		}
	}
}

// findFiles finds files matching a pattern in the directory.
func findFiles(dir, pattern string) []string {
	var files []string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			// Skip common directories
			base := filepath.Base(path)
			if base == "node_modules" || base == "vendor" || base == ".git" || base == "target" {
				return filepath.SkipDir
			}
		}
		if !info.IsDir() && filepath.Base(path) == pattern {
			rel, _ := filepath.Rel(dir, path)
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// isKeyDependency checks if a dependency is a key framework.
func isKeyDependency(dep string) bool {
	keyDeps := map[string]bool{
		"react": true, "vue": true, "angular": true, "svelte": true,
		"next": true, "nuxt": true, "express": true, "fastify": true,
		"nestjs": true, "typescript": true, "vite": true, "webpack": true,
	}
	return keyDeps[dep]
}

// String returns a human-readable string for the project type.
func (t ProjectType) String() string {
	switch t {
	case ProjectTypeGo:
		return "Go"
	case ProjectTypeNode:
		return "Node.js"
	case ProjectTypeRust:
		return "Rust"
	case ProjectTypePython:
		return "Python"
	case ProjectTypeJava:
		return "Java"
	case ProjectTypeRuby:
		return "Ruby"
	case ProjectTypePHP:
		return "PHP"
	default:
		return "Unknown"
	}
}
