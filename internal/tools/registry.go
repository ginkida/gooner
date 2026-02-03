package tools

import (
	"fmt"
	"log"
	"sync"

	"google.golang.org/genai"
)

// Registry manages the collection of available tools.
type Registry struct {
	tools map[string]Tool
	mu    sync.RWMutex
}

// NewRegistry creates a new tool registry.
func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

// Get retrieves a tool by name (read-optimized with RLock).
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tool, ok := r.tools[name]
	return tool, ok
}

// List returns all registered tools (read-optimized).
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()

	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// Names returns the names of all registered tools (read-optimized).
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// Declarations returns all tool declarations for Gemini (read-optimized).
func (r *Registry) Declarations() []*genai.FunctionDeclaration {
	r.mu.RLock()
	defer r.mu.RUnlock()

	declarations := make([]*genai.FunctionDeclaration, 0, len(r.tools))
	for _, tool := range r.tools {
		declarations = append(declarations, tool.Declaration())
	}
	return declarations
}

// Register adds a tool to the registry.
func (r *Registry) Register(tool Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name := tool.Name()
	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool already registered: %s", name)
	}

	r.tools[name] = tool
	return nil
}

// MustRegister adds a tool to the registry and logs a warning on error.
func (r *Registry) MustRegister(tool Tool) {
	if err := r.Register(tool); err != nil {
		log.Printf("WARNING: failed to register tool %q: %v", tool.Name(), err)
	}
}

// GeminiTools returns the tools in Gemini format.
func (r *Registry) GeminiTools() []*genai.Tool {
	return []*genai.Tool{
		{
			FunctionDeclarations: r.Declarations(),
		},
	}
}

// DefaultRegistry creates a registry with all default tools.
func DefaultRegistry(workDir string) *Registry {
	r := NewRegistry()

	// Register all tools
	r.MustRegister(NewReadTool())
	r.MustRegister(NewWriteTool(workDir))
	r.MustRegister(NewEditTool())
	r.MustRegister(NewBashTool(workDir))
	r.MustRegister(NewGlobTool(workDir))
	r.MustRegister(NewGrepTool(workDir))
	r.MustRegister(NewTodoTool())
	r.MustRegister(NewListDirTool(workDir))
	r.MustRegister(NewDiffTool())
	r.MustRegister(NewTreeTool(workDir))
	r.MustRegister(NewEnvTool())
	r.MustRegister(NewAskUserTool())
	r.MustRegister(NewTaskOutputTool())
	r.MustRegister(NewTaskStopTool())
	r.MustRegister(NewWebFetchTool())
	r.MustRegister(NewWebSearchTool())
	r.MustRegister(NewUndoTool())
	r.MustRegister(NewTaskTool())
	r.MustRegister(NewKillShellTool())
	r.MustRegister(NewMemoryTool())
	r.MustRegister(NewEnterPlanModeTool())
	r.MustRegister(NewUpdatePlanProgressTool())
	r.MustRegister(NewGetPlanStatusTool())
	r.MustRegister(NewExitPlanModeTool())
	r.MustRegister(NewUndoPlanTool())
	r.MustRegister(NewRedoPlanTool())
	r.MustRegister(NewBatchTool(workDir))
	r.MustRegister(NewRefactorTool())
	r.MustRegister(NewCodeGraphTool())
	r.MustRegister(NewPatternSearchTool())
	r.MustRegister(NewToolsListTool(r))
	r.MustRegister(NewRequestToolTool())
	r.MustRegister(NewAskAgentTool())

	// Plan verification tool (contract merged into plan)
	r.MustRegister(NewVerifyPlanTool())

	// File operation tools
	r.MustRegister(NewCopyTool(workDir))
	r.MustRegister(NewMoveTool(workDir))
	r.MustRegister(NewDeleteTool(workDir))
	r.MustRegister(NewMkdirTool(workDir))

	// Git tools
	r.MustRegister(NewGitLogTool(workDir))
	r.MustRegister(NewGitBlameTool(workDir))
	r.MustRegister(NewGitDiffTool(workDir))
	r.MustRegister(NewGitStatusTool(workDir))
	r.MustRegister(NewGitAddTool(workDir))
	r.MustRegister(NewGitCommitTool(workDir))

	// SSH tool
	r.MustRegister(NewSSHTool())

	// Coordination tool
	r.MustRegister(NewCoordinateTool())

	// Shared memory tool (Phase 2)
	r.MustRegister(NewSharedMemoryTool())

	// Agent Scratchpad tool (Phase 7)
	r.MustRegister(NewUpdateScratchpadTool(nil))

	return r
}
