package tools

import (
	"sync"
)

// ToolFactory is a function that creates a tool instance.
type ToolFactory func() Tool

// ToolEntry holds a tool factory and its lazy-loaded instance.
// Thread-safe via sync.Once for instance creation.
type ToolEntry struct {
	factory     ToolFactory
	instance    Tool
	once        sync.Once
	configFuncs []func(Tool)
	configMu    sync.Mutex
}

// NewToolEntry creates a new tool entry with the given factory.
func NewToolEntry(factory ToolFactory) *ToolEntry {
	return &ToolEntry{
		factory:     factory,
		configFuncs: make([]func(Tool), 0),
	}
}

// Get returns the tool instance, creating it if necessary.
// This is thread-safe and will only create the instance once.
func (e *ToolEntry) Get() Tool {
	e.once.Do(func() {
		e.instance = e.factory()

		// Apply all pending configurations
		e.configMu.Lock()
		configs := e.configFuncs
		e.configMu.Unlock()

		for _, cfg := range configs {
			cfg(e.instance)
		}
	})
	return e.instance
}

// Configure adds a configuration function to be applied to the tool.
// If the tool is already instantiated, the config is applied immediately.
// If not, it will be applied when Get() is called.
func (e *ToolEntry) Configure(cfg func(Tool)) {
	e.configMu.Lock()
	defer e.configMu.Unlock()

	// Check if instance already exists (without triggering creation)
	if e.instance != nil {
		// Apply immediately
		cfg(e.instance)
	} else {
		// Queue for later
		e.configFuncs = append(e.configFuncs, cfg)
	}
}

// IsInstantiated returns true if the tool has been instantiated.
func (e *ToolEntry) IsInstantiated() bool {
	e.configMu.Lock()
	defer e.configMu.Unlock()
	return e.instance != nil
}

// ConfigurableToolEntry provides typed configuration for specific tool types.
// This is a generic wrapper for type-safe configuration.

// ToolConfigurer is an interface for tools that can be configured.
type ToolConfigurer[T Tool] interface {
	Configure(cfg func(T))
}

// TypedToolEntry wraps ToolEntry with type-safe configuration.
type TypedToolEntry[T Tool] struct {
	*ToolEntry
}

// NewTypedToolEntry creates a typed tool entry.
func NewTypedToolEntry[T Tool](factory func() T) *TypedToolEntry[T] {
	return &TypedToolEntry[T]{
		ToolEntry: NewToolEntry(func() Tool { return factory() }),
	}
}

// Configure adds a typed configuration function.
func (e *TypedToolEntry[T]) Configure(cfg func(T)) {
	e.ToolEntry.Configure(func(t Tool) {
		if typed, ok := t.(T); ok {
			cfg(typed)
		}
	})
}

// Get returns the typed tool instance.
func (e *TypedToolEntry[T]) Get() T {
	return e.ToolEntry.Get().(T)
}

// ToolEntryMap is a map of tool names to their entries.
type ToolEntryMap map[string]*ToolEntry

// GetOrCreate returns an existing entry or creates a new one.
func (m ToolEntryMap) GetOrCreate(name string, factory ToolFactory) *ToolEntry {
	if entry, ok := m[name]; ok {
		return entry
	}
	entry := NewToolEntry(factory)
	m[name] = entry
	return entry
}

// ToolFactoryRegistry holds factories for all tools.
// This allows registering tool factories without instantiating tools.
type ToolFactoryRegistry struct {
	factories map[string]ToolFactory
	mu        sync.RWMutex
}

// NewToolFactoryRegistry creates a new factory registry.
func NewToolFactoryRegistry() *ToolFactoryRegistry {
	return &ToolFactoryRegistry{
		factories: make(map[string]ToolFactory),
	}
}

// Register adds a tool factory to the registry.
func (r *ToolFactoryRegistry) Register(name string, factory ToolFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
}

// Get returns the factory for a tool name.
func (r *ToolFactoryRegistry) Get(name string) (ToolFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	factory, ok := r.factories[name]
	return factory, ok
}

// Names returns all registered tool names.
func (r *ToolFactoryRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	return names
}

// CreateAll creates all tools from registered factories.
func (r *ToolFactoryRegistry) CreateAll() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.factories))
	for _, factory := range r.factories {
		tools = append(tools, factory())
	}
	return tools
}
