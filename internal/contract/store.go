package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Store handles YAML persistence for contracts.
type Store struct {
	projectDir string
	globalDir  string
	contracts  map[string]*Contract
	mu         sync.RWMutex
}

// NewStore creates a new contract store.
// projectDir is the project's .gokin/contracts/ directory.
// globalDir is the global config directory for lessons.
func NewStore(workDir, configDir string) (*Store, error) {
	projectDir := filepath.Join(workDir, ".gokin", "contracts")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create contracts directory: %w", err)
	}

	globalDir := configDir
	if globalDir != "" {
		if err := os.MkdirAll(globalDir, 0755); err != nil {
			// Non-fatal: global lessons won't be available
			globalDir = ""
		}
	}

	s := &Store{
		projectDir: projectDir,
		globalDir:  globalDir,
		contracts:  make(map[string]*Contract),
	}

	// Load existing contracts
	if err := s.loadAll(); err != nil {
		// Non-fatal: start with empty contracts
		_ = err
	}

	return s, nil
}

// Save persists a contract to disk.
func (s *Store) Save(c *Contract) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("failed to marshal contract: %w", err)
	}

	path := filepath.Join(s.projectDir, c.ID+".yaml")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write contract file: %w", err)
	}

	s.contracts[c.ID] = c
	return nil
}

// Load retrieves a contract by ID.
func (s *Store) Load(id string) (*Contract, error) {
	s.mu.RLock()
	if c, ok := s.contracts[id]; ok {
		s.mu.RUnlock()
		return c, nil
	}
	s.mu.RUnlock()

	// Try loading from disk
	path := filepath.Join(s.projectDir, id+".yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("contract not found: %s", id)
	}

	var c Contract
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse contract: %w", err)
	}

	s.mu.Lock()
	s.contracts[c.ID] = &c
	s.mu.Unlock()

	return &c, nil
}

// List returns all contracts.
func (s *Store) List() ([]*Contract, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*Contract, 0, len(s.contracts))
	for _, c := range s.contracts {
		result = append(result, c)
	}
	return result, nil
}

// Delete removes a contract.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := filepath.Join(s.projectDir, id+".yaml")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete contract: %w", err)
	}

	delete(s.contracts, id)
	return nil
}

// FindByName finds a contract by name (case-insensitive).
func (s *Store) FindByName(name string) *Contract {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nameLower := strings.ToLower(name)
	for _, c := range s.contracts {
		if strings.ToLower(c.Name) == nameLower {
			return c
		}
	}
	return nil
}

// SaveGlobalLesson saves a lesson to the global lessons index.
func (s *Store) SaveGlobalLesson(l *Lesson) error {
	if s.globalDir == "" {
		return fmt.Errorf("global config directory not available")
	}

	lessons, _ := s.LoadGlobalLessons()
	lessons = append(lessons, l)

	data, err := yaml.Marshal(lessons)
	if err != nil {
		return fmt.Errorf("failed to marshal lessons: %w", err)
	}

	path := filepath.Join(s.globalDir, "contract_lessons.yaml")
	return os.WriteFile(path, data, 0644)
}

// LoadGlobalLessons loads all global lessons.
func (s *Store) LoadGlobalLessons() ([]*Lesson, error) {
	if s.globalDir == "" {
		return nil, nil
	}

	path := filepath.Join(s.globalDir, "contract_lessons.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil // No lessons file is not an error
	}

	var lessons []*Lesson
	if err := yaml.Unmarshal(data, &lessons); err != nil {
		return nil, fmt.Errorf("failed to parse lessons: %w", err)
	}

	return lessons, nil
}

// loadAll loads all contracts from the project directory.
func (s *Store) loadAll() error {
	entries, err := os.ReadDir(s.projectDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}

		path := filepath.Join(s.projectDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}

		var c Contract
		if err := yaml.Unmarshal(data, &c); err != nil {
			continue
		}

		s.contracts[c.ID] = &c
	}

	return nil
}
