package tools

import (
	"google.golang.org/genai"
)

// Static tool declarations for lazy loading.
// These are used to register tools with AI models without instantiating the tools.

// ReadToolDeclaration returns the declaration for the read tool.
func ReadToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "read",
		Description: "Reads a file from the filesystem and returns its contents with line numbers. PREFER this over bash cat/head/tail. Use to understand file contents, find specific code, check configuration.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The absolute path to the file to read",
				},
				"offset": {
					Type:        genai.TypeInteger,
					Description: "The line number to start reading from (1-indexed). Optional.",
				},
				"limit": {
					Type:        genai.TypeInteger,
					Description: "The maximum number of lines to read. Optional, defaults to 2000.",
				},
			},
			Required: []string{"file_path"},
		},
	}
}

// WriteToolDeclaration returns the declaration for the write tool.
func WriteToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "write",
		Description: "Creates or overwrites a file with new content. Use ONLY for new files or full rewrites. For targeted modifications of existing files, use 'edit' instead.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The absolute path to the file to write",
				},
				"content": {
					Type:        genai.TypeString,
					Description: "The content to write to the file",
				},
			},
			Required: []string{"file_path", "content"},
		},
	}
}

// EditToolDeclaration returns the declaration for the edit tool.
func EditToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "edit",
		Description: "Performs string replacement in a file. Supports three modes: (1) old_string/new_string for exact match, (2) regex=true for regex replacement, (3) line_start/line_end/new_string for line-based replacement. PREFER this over 'write' for modifying existing files.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The absolute path to the file to edit",
				},
				"old_string": {
					Type:        genai.TypeString,
					Description: "The text to find and replace",
				},
				"new_string": {
					Type:        genai.TypeString,
					Description: "The text to replace with (must be different from old_string)",
				},
				"replace_all": {
					Type:        genai.TypeBoolean,
					Description: "If true, replace all occurrences. If false (default), old_string must be unique.",
				},
				"regex": {
					Type:        genai.TypeBoolean,
					Description: "If true, treat old_string as a regular expression pattern.",
				},
				"line_start": {
					Type:        genai.TypeInteger,
					Description: "Start line (1-indexed). Alternative to old_string: replaces lines line_start..line_end with new_string.",
				},
				"line_end": {
					Type:        genai.TypeInteger,
					Description: "End line (1-indexed, inclusive). Used with line_start.",
				},
				"edits": {
					Type:        genai.TypeArray,
					Description: "Array of {old_string, new_string} pairs for multiple edits in one call.",
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"old_string": {
								Type:        genai.TypeString,
								Description: "The text to find",
							},
							"new_string": {
								Type:        genai.TypeString,
								Description: "The text to replace with",
							},
						},
						Required: []string{"old_string", "new_string"},
					},
				},
			},
			Required: []string{"file_path"},
		},
	}
}

// BashToolDeclaration returns the declaration for the bash tool.
func BashToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "bash",
		Description: "Executes a bash command and returns the output. Use for builds, tests, git commands, and system operations. Do NOT use for file operations that have dedicated tools: use 'read' instead of cat, 'grep' instead of grep/rg, 'glob' instead of find/ls, 'edit' instead of sed.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"command": {
					Type:        genai.TypeString,
					Description: "The bash command to execute",
				},
				"description": {
					Type:        genai.TypeString,
					Description: "A brief description of what the command does",
				},
				"run_in_background": {
					Type:        genai.TypeBoolean,
					Description: "If true, run the command in background and return task ID immediately",
				},
			},
			Required: []string{"command"},
		},
	}
}

// GlobToolDeclaration returns the declaration for the glob tool.
func GlobToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "glob",
		Description: "Finds files matching a glob pattern. Returns file paths sorted by modification time.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern": {
					Type:        genai.TypeString,
					Description: "The glob pattern to match (e.g., '**/*.go', 'src/**/*.ts')",
				},
				"path": {
					Type:        genai.TypeString,
					Description: "The directory to search in. Defaults to current working directory.",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

// GrepToolDeclaration returns the declaration for the grep tool.
func GrepToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "grep",
		Description: "Searches for patterns in files using ripgrep-like syntax.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern": {
					Type:        genai.TypeString,
					Description: "The regex pattern to search for",
				},
				"path": {
					Type:        genai.TypeString,
					Description: "File or directory to search in",
				},
				"include": {
					Type:        genai.TypeString,
					Description: "Glob pattern to filter files (e.g., '*.go')",
				},
				"context": {
					Type:        genai.TypeInteger,
					Description: "Number of context lines to show around matches",
				},
				"case_sensitive": {
					Type:        genai.TypeBoolean,
					Description: "Whether the search is case sensitive (default: true)",
				},
			},
			Required: []string{"pattern"},
		},
	}
}

// TodoToolDeclaration returns the declaration for the todo tool.
func TodoToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "todo",
		Description: "Manages a todo list for tracking tasks during the session.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: add, remove, complete, list",
				},
				"item": {
					Type:        genai.TypeString,
					Description: "The todo item text (for add/remove)",
				},
				"id": {
					Type:        genai.TypeInteger,
					Description: "The todo item ID (for remove/complete)",
				},
			},
			Required: []string{"action"},
		},
	}
}

// ListDirToolDeclaration returns the declaration for the list_dir tool.
func ListDirToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "list_dir",
		Description: "Lists contents of a directory with file sizes and types.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The directory path to list",
				},
				"show_hidden": {
					Type:        genai.TypeBoolean,
					Description: "Whether to show hidden files (default: false)",
				},
			},
			Required: []string{"path"},
		},
	}
}

// DiffToolDeclaration returns the declaration for the diff tool.
func DiffToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "diff",
		Description: "Shows differences between two texts or files.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"old_text": {
					Type:        genai.TypeString,
					Description: "The original text",
				},
				"new_text": {
					Type:        genai.TypeString,
					Description: "The new text to compare",
				},
			},
			Required: []string{"old_text", "new_text"},
		},
	}
}

// TreeToolDeclaration returns the declaration for the tree tool.
func TreeToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "tree",
		Description: "Shows directory structure as a tree.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The directory path to display as tree",
				},
				"max_depth": {
					Type:        genai.TypeInteger,
					Description: "Maximum depth to display (default: 3)",
				},
			},
			Required: []string{"path"},
		},
	}
}

// EnvToolDeclaration returns the declaration for the env tool.
func EnvToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "env",
		Description: "Returns environment information (working directory, OS, etc.).",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{},
		},
	}
}

// AskUserToolDeclaration returns the declaration for the ask_user tool.
func AskUserToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "ask_user",
		Description: "Asks the user a question and waits for a response.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"question": {
					Type:        genai.TypeString,
					Description: "The question to ask the user",
				},
			},
			Required: []string{"question"},
		},
	}
}

// TaskOutputToolDeclaration returns the declaration for the task_output tool.
func TaskOutputToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "task_output",
		Description: "Gets output from a background task by ID.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"task_id": {
					Type:        genai.TypeString,
					Description: "The task ID to get output from",
				},
				"wait": {
					Type:        genai.TypeBoolean,
					Description: "Whether to wait for task completion (default: true)",
				},
			},
			Required: []string{"task_id"},
		},
	}
}

// TaskStopToolDeclaration returns the declaration for the task_stop tool.
func TaskStopToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "task_stop",
		Description: "Stops a running background task.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"task_id": {
					Type:        genai.TypeString,
					Description: "The task ID to stop",
				},
			},
			Required: []string{"task_id"},
		},
	}
}

// WebFetchToolDeclaration returns the declaration for the web_fetch tool.
func WebFetchToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "web_fetch",
		Description: "Fetches content from a URL and returns it.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"url": {
					Type:        genai.TypeString,
					Description: "The URL to fetch content from",
				},
				"extract_text": {
					Type:        genai.TypeBoolean,
					Description: "Whether to extract text only (default: true)",
				},
			},
			Required: []string{"url"},
		},
	}
}

// WebSearchToolDeclaration returns the declaration for the web_search tool.
func WebSearchToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "web_search",
		Description: "Searches the web and returns results.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        genai.TypeString,
					Description: "The search query",
				},
				"num_results": {
					Type:        genai.TypeInteger,
					Description: "Number of results to return (default: 5)",
				},
			},
			Required: []string{"query"},
		},
	}
}

// TaskToolDeclaration returns the declaration for the task tool.
func TaskToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "task",
		Description: "Spawns a sub-agent to perform a specific task.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"prompt": {
					Type:        genai.TypeString,
					Description: "The task description for the sub-agent",
				},
				"subagent_type": {
					Type:        genai.TypeString,
					Description: "The type of sub-agent to spawn (explore, bash, general, plan, guide)",
				},
				"run_in_background": {
					Type:        genai.TypeBoolean,
					Description: "Whether to run the task in background",
				},
			},
			Required: []string{"prompt", "subagent_type"},
		},
	}
}

// KillShellToolDeclaration returns the declaration for the kill_shell tool.
func KillShellToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "kill_shell",
		Description: "Kills a running shell/task by ID.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"task_id": {
					Type:        genai.TypeString,
					Description: "The task ID to kill",
				},
			},
			Required: []string{"task_id"},
		},
	}
}

// MemoryToolDeclaration returns the declaration for the memory tool.
func MemoryToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "memory",
		Description: "Manages persistent memory across sessions.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: add, get, search, list, remove",
				},
				"key": {
					Type:        genai.TypeString,
					Description: "The memory key",
				},
				"value": {
					Type:        genai.TypeString,
					Description: "The memory value (for add)",
				},
				"query": {
					Type:        genai.TypeString,
					Description: "Search query (for search)",
				},
			},
			Required: []string{"action"},
		},
	}
}

// EnterPlanModeToolDeclaration returns the declaration for the enter_plan_mode tool.
func EnterPlanModeToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "enter_plan_mode",
		Description: "Enters planning mode to create a structured plan before execution.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"title": {
					Type:        genai.TypeString,
					Description: "The title of the plan",
				},
				"description": {
					Type:        genai.TypeString,
					Description: "A description of what the plan will accomplish",
				},
			},
			Required: []string{"title"},
		},
	}
}

// UpdatePlanProgressToolDeclaration returns the declaration for the update_plan_progress tool.
func UpdatePlanProgressToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "update_plan_progress",
		Description: "Updates the progress of the current plan.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"step_id": {
					Type:        genai.TypeString,
					Description: "The ID of the step to update",
				},
				"status": {
					Type:        genai.TypeString,
					Description: "The new status (pending, in_progress, completed, failed)",
				},
				"notes": {
					Type:        genai.TypeString,
					Description: "Optional notes about the progress",
				},
			},
			Required: []string{"step_id", "status"},
		},
	}
}

// GetPlanStatusToolDeclaration returns the declaration for the get_plan_status tool.
func GetPlanStatusToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "get_plan_status",
		Description: "Gets the current status of the active plan.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{},
		},
	}
}

// ExitPlanModeToolDeclaration returns the declaration for the exit_plan_mode tool.
func ExitPlanModeToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "exit_plan_mode",
		Description: "Exits planning mode and optionally approves the plan for execution.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"approve": {
					Type:        genai.TypeBoolean,
					Description: "Whether to approve the plan for execution",
				},
			},
			Required: []string{"approve"},
		},
	}
}

// UndoPlanToolDeclaration returns the declaration for the undo_plan tool.
func UndoPlanToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "undo_plan",
		Description: "Undoes the last plan step.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{},
		},
	}
}

// RedoPlanToolDeclaration returns the declaration for the redo_plan tool.
func RedoPlanToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "redo_plan",
		Description: "Redoes the last undone plan step.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{},
		},
	}
}

// BatchToolDeclaration returns the declaration for the batch tool.
func BatchToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "batch",
		Description: "Applies multiple file edits atomically as a batch.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"operations": {
					Type:        genai.TypeArray,
					Description: "List of edit operations to apply",
					Items: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"file_path":   {Type: genai.TypeString},
							"old_string":  {Type: genai.TypeString},
							"new_string":  {Type: genai.TypeString},
							"replace_all": {Type: genai.TypeBoolean},
						},
					},
				},
			},
			Required: []string{"operations"},
		},
	}
}

// RefactorToolDeclaration returns the declaration for the refactor tool.
func RefactorToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "refactor",
		Description: "Refactors code across multiple files (rename, extract, etc.).",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"operation": {
					Type:        genai.TypeString,
					Description: "The refactoring operation (rename, extract_function, inline, etc.)",
				},
				"target": {
					Type:        genai.TypeString,
					Description: "The target identifier to refactor",
				},
				"new_name": {
					Type:        genai.TypeString,
					Description: "The new name (for rename operations)",
				},
				"scope": {
					Type:        genai.TypeString,
					Description: "The scope of the refactoring (file, package, project)",
				},
			},
			Required: []string{"operation", "target"},
		},
	}
}

// CodeGraphToolDeclaration returns the declaration for the code_graph tool.
func CodeGraphToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "code_graph",
		Description: "Analyzes code structure and dependencies.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: dependencies, references, callers, callees",
				},
				"symbol": {
					Type:        genai.TypeString,
					Description: "The symbol to analyze",
				},
				"file_path": {
					Type:        genai.TypeString,
					Description: "The file containing the symbol",
				},
			},
			Required: []string{"action", "symbol"},
		},
	}
}

// ToolsListToolDeclaration returns the declaration for the tools_list tool.
func ToolsListToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "tools_list",
		Description: "Returns a list of all available tools in the system with their descriptions.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{},
		},
	}
}

// RequestToolToolDeclaration returns the declaration for the request_tool tool.
func RequestToolToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "request_tool",
		Description: "Requests a tool that is not in the current toolkit.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"tool_name": {
					Type:        genai.TypeString,
					Description: "The name of the tool to request",
				},
				"reason": {
					Type:        genai.TypeString,
					Description: "The reason for requesting the tool",
				},
			},
			Required: []string{"tool_name"},
		},
	}
}

// AskAgentToolDeclaration returns the declaration for the ask_agent tool.
func AskAgentToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "ask_agent",
		Description: "Asks another agent for assistance.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"agent_type": {
					Type:        genai.TypeString,
					Description: "The type of agent to ask",
				},
				"question": {
					Type:        genai.TypeString,
					Description: "The question to ask",
				},
			},
			Required: []string{"agent_type", "question"},
		},
	}
}

// CopyToolDeclaration returns the declaration for the copy tool.
func CopyToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "copy",
		Description: "Copies a file or directory.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"source": {
					Type:        genai.TypeString,
					Description: "The source path",
				},
				"destination": {
					Type:        genai.TypeString,
					Description: "The destination path",
				},
			},
			Required: []string{"source", "destination"},
		},
	}
}

// MoveToolDeclaration returns the declaration for the move tool.
func MoveToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "move",
		Description: "Moves (renames) a file or directory.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"source": {
					Type:        genai.TypeString,
					Description: "The source path",
				},
				"destination": {
					Type:        genai.TypeString,
					Description: "The destination path",
				},
			},
			Required: []string{"source", "destination"},
		},
	}
}

// DeleteToolDeclaration returns the declaration for the delete tool.
func DeleteToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "delete",
		Description: "Deletes a file or directory.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path to delete",
				},
				"recursive": {
					Type:        genai.TypeBoolean,
					Description: "Whether to delete recursively (for directories)",
				},
			},
			Required: []string{"path"},
		},
	}
}

// MkdirToolDeclaration returns the declaration for the mkdir tool.
func MkdirToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "mkdir",
		Description: "Creates a directory.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The path of the directory to create",
				},
				"parents": {
					Type:        genai.TypeBoolean,
					Description: "Whether to create parent directories as needed",
				},
			},
			Required: []string{"path"},
		},
	}
}

// GitLogToolDeclaration returns the declaration for the git_log tool.
func GitLogToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_log",
		Description: "Shows git commit history.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"limit": {
					Type:        genai.TypeInteger,
					Description: "Maximum number of commits to show (default: 10)",
				},
				"path": {
					Type:        genai.TypeString,
					Description: "Path to filter commits by",
				},
				"author": {
					Type:        genai.TypeString,
					Description: "Author to filter commits by",
				},
			},
		},
	}
}

// GitBlameToolDeclaration returns the declaration for the git_blame tool.
func GitBlameToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_blame",
		Description: "Shows who last modified each line of a file.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"file_path": {
					Type:        genai.TypeString,
					Description: "The file to blame",
				},
				"start_line": {
					Type:        genai.TypeInteger,
					Description: "Starting line number",
				},
				"end_line": {
					Type:        genai.TypeInteger,
					Description: "Ending line number",
				},
			},
			Required: []string{"file_path"},
		},
	}
}

// GitDiffToolDeclaration returns the declaration for the git_diff tool.
func GitDiffToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_diff",
		Description: "Shows git diff between commits or working tree.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"ref1": {
					Type:        genai.TypeString,
					Description: "First reference (commit, branch, or tag)",
				},
				"ref2": {
					Type:        genai.TypeString,
					Description: "Second reference (commit, branch, or tag)",
				},
				"path": {
					Type:        genai.TypeString,
					Description: "Path to filter diff by",
				},
				"staged": {
					Type:        genai.TypeBoolean,
					Description: "Show only staged changes",
				},
			},
		},
	}
}

// GitStatusToolDeclaration returns the declaration for the git_status tool.
func GitStatusToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_status",
		Description: "Shows the working tree status.",
		Parameters: &genai.Schema{
			Type:       genai.TypeObject,
			Properties: map[string]*genai.Schema{},
		},
	}
}

// GitAddToolDeclaration returns the declaration for the git_add tool.
func GitAddToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_add",
		Description: "Adds files to the staging area.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"paths": {
					Type:        genai.TypeArray,
					Description: "List of file paths to add",
					Items:       &genai.Schema{Type: genai.TypeString},
				},
				"all": {
					Type:        genai.TypeBoolean,
					Description: "Add all changes",
				},
			},
		},
	}
}

// GitCommitToolDeclaration returns the declaration for the git_commit tool.
func GitCommitToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_commit",
		Description: "Creates a git commit.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"message": {
					Type:        genai.TypeString,
					Description: "The commit message",
				},
				"amend": {
					Type:        genai.TypeBoolean,
					Description: "Amend the previous commit",
				},
			},
			Required: []string{"message"},
		},
	}
}

// SSHToolDeclaration returns the declaration for the ssh tool.
func SSHToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "ssh",
		Description: "Executes a command on a remote server via SSH.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"host": {
					Type:        genai.TypeString,
					Description: "The remote host (user@host or host)",
				},
				"command": {
					Type:        genai.TypeString,
					Description: "The command to execute",
				},
			},
			Required: []string{"host", "command"},
		},
	}
}

// CoordinateToolDeclaration returns the declaration for the coordinate tool.
func CoordinateToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "coordinate",
		Description: "Coordinates multiple agents to work on a task.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"task": {
					Type:        genai.TypeString,
					Description: "The task to coordinate",
				},
				"agents": {
					Type:        genai.TypeArray,
					Description: "List of agent types to coordinate",
					Items:       &genai.Schema{Type: genai.TypeString},
				},
			},
			Required: []string{"task"},
		},
	}
}

// SharedMemoryToolDeclaration returns the declaration for the shared_memory tool.
func SharedMemoryToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "shared_memory",
		Description: "Accesses shared memory between agents.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Action to perform: read, write, delete, list",
				},
				"key": {
					Type:        genai.TypeString,
					Description: "The memory key",
				},
				"value": {
					Type:        genai.TypeString,
					Description: "The value to store (for write)",
				},
			},
			Required: []string{"action"},
		},
	}
}

// UpdateScratchpadToolDeclaration returns the declaration for the update_scratchpad tool.
func UpdateScratchpadToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "update_scratchpad",
		Description: "Updates the agent's scratchpad with notes.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"content": {
					Type:        genai.TypeString,
					Description: "The content to write to the scratchpad",
				},
				"append": {
					Type:        genai.TypeBoolean,
					Description: "Whether to append to existing content",
				},
			},
			Required: []string{"content"},
		},
	}
}

// SemanticSearchToolDeclaration returns the declaration for the semantic_search tool.
func SemanticSearchToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "semantic_search",
		Description: "Searches code using semantic similarity.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        genai.TypeString,
					Description: "The semantic search query",
				},
				"top_k": {
					Type:        genai.TypeInteger,
					Description: "Number of results to return (default: 10)",
				},
			},
			Required: []string{"query"},
		},
	}
}

// VerifyCodeToolDeclaration returns the declaration for the verify_code tool.
func VerifyCodeToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "verify_code",
		Description: "Automatically verifies code correctness in the project. Detects project type and runs relevant checks.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "The directory to verify (defaults to project root)",
				},
			},
		},
	}
}

// CheckImpactToolDeclaration returns the declaration for the check_impact tool.
func CheckImpactToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "check_impact",
		Description: "Blast Radius Analysis tool. Finds all usages and potential impacts of changing a symbol.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"symbol": {
					Type:        genai.TypeString,
					Description: "The symbol name to analyze",
				},
			},
			Required: []string{"symbol"},
		},
	}
}

// MemorizeToolDeclaration returns the declaration for the memorize tool.
func MemorizeToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "memorize",
		Description: "Saves project-specific knowledge to persistent memory.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"type": {
					Type:        genai.TypeString,
					Description: "Type of information: 'fact', 'preference', 'convention', 'pattern'",
					Enum:        []string{"fact", "preference", "convention", "pattern"},
				},
				"key": {
					Type:        genai.TypeString,
					Description: "Name for the knowledge",
				},
				"content": {
					Type:        genai.TypeString,
					Description: "Actual fact or preference",
				},
			},
			Required: []string{"type", "key", "content"},
		},
	}
}

// GetAllDeclarations returns all tool declarations for lazy registry.
func GetAllDeclarations() map[string]*genai.FunctionDeclaration {
	return map[string]*genai.FunctionDeclaration{
		"read":                 ReadToolDeclaration(),
		"write":                WriteToolDeclaration(),
		"edit":                 EditToolDeclaration(),
		"bash":                 BashToolDeclaration(),
		"glob":                 GlobToolDeclaration(),
		"grep":                 GrepToolDeclaration(),
		"todo":                 TodoToolDeclaration(),
		"list_dir":             ListDirToolDeclaration(),
		"diff":                 DiffToolDeclaration(),
		"tree":                 TreeToolDeclaration(),
		"env":                  EnvToolDeclaration(),
		"ask_user":             AskUserToolDeclaration(),
		"task_output":          TaskOutputToolDeclaration(),
		"task_stop":            TaskStopToolDeclaration(),
		"web_fetch":            WebFetchToolDeclaration(),
		"web_search":           WebSearchToolDeclaration(),
		"task":                 TaskToolDeclaration(),
		"kill_shell":           KillShellToolDeclaration(),
		"memory":               MemoryToolDeclaration(),
		"enter_plan_mode":      EnterPlanModeToolDeclaration(),
		"update_plan_progress": UpdatePlanProgressToolDeclaration(),
		"get_plan_status":      GetPlanStatusToolDeclaration(),
		"exit_plan_mode":       ExitPlanModeToolDeclaration(),
		"undo_plan":            UndoPlanToolDeclaration(),
		"redo_plan":            RedoPlanToolDeclaration(),
		"batch":                BatchToolDeclaration(),
		"refactor":             RefactorToolDeclaration(),
		"code_graph":           CodeGraphToolDeclaration(),
		"check_impact":         CheckImpactToolDeclaration(),
		"memorize":             MemorizeToolDeclaration(),
		"tools_list":           ToolsListToolDeclaration(),
		"request_tool":         RequestToolToolDeclaration(),
		"ask_agent":            AskAgentToolDeclaration(),
		"copy":                 CopyToolDeclaration(),
		"move":                 MoveToolDeclaration(),
		"delete":               DeleteToolDeclaration(),
		"mkdir":                MkdirToolDeclaration(),
		"git_log":              GitLogToolDeclaration(),
		"git_blame":            GitBlameToolDeclaration(),
		"git_diff":             GitDiffToolDeclaration(),
		"git_status":           GitStatusToolDeclaration(),
		"git_add":              GitAddToolDeclaration(),
		"git_commit":           GitCommitToolDeclaration(),
		"ssh":                  SSHToolDeclaration(),
		"coordinate":           CoordinateToolDeclaration(),
		"shared_memory":        SharedMemoryToolDeclaration(),
		"update_scratchpad":    UpdateScratchpadToolDeclaration(),
		"semantic_search":      SemanticSearchToolDeclaration(),
		"verify_code":          VerifyCodeToolDeclaration(),
		"pin_context":          PinContextToolDeclaration(),
		"history_search":       HistorySearchToolDeclaration(),
		"run_tests":            RunTestsToolDeclaration(),
		"git_branch":           GitBranchToolDeclaration(),
		"git_pr":               GitPRToolDeclaration(),
	}
}

// RunTestsToolDeclaration returns the declaration for the run_tests tool.
func RunTestsToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "run_tests",
		Description: "Runs project tests with automatic framework detection (Go, Python, Node, Rust). Parses output, reports failures with context, and provides coverage summary.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"path": {
					Type:        genai.TypeString,
					Description: "Path to run tests in (default: working directory)",
				},
				"filter": {
					Type:        genai.TypeString,
					Description: "Test name filter/pattern",
				},
				"verbose": {
					Type:        genai.TypeBoolean,
					Description: "Show verbose output",
				},
				"coverage": {
					Type:        genai.TypeBoolean,
					Description: "Run with coverage reporting",
				},
				"framework": {
					Type:        genai.TypeString,
					Description: "Force framework: 'go', 'pytest', 'jest', 'cargo', 'auto'",
					Enum:        []string{"auto", "go", "pytest", "jest", "cargo"},
				},
			},
		},
	}
}

// GitBranchToolDeclaration returns the declaration for the git_branch tool.
func GitBranchToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_branch",
		Description: "Manages git branches: list, create, delete, switch (checkout), and merge branches.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "Branch action: 'list', 'create', 'delete', 'switch', 'merge', 'current'",
					Enum:        []string{"list", "create", "delete", "switch", "merge", "current"},
				},
				"name": {
					Type:        genai.TypeString,
					Description: "Branch name",
				},
				"from": {
					Type:        genai.TypeString,
					Description: "Base branch to create from",
				},
				"force": {
					Type:        genai.TypeBoolean,
					Description: "Force operation",
				},
				"all": {
					Type:        genai.TypeBoolean,
					Description: "Show remote branches (for list)",
				},
			},
			Required: []string{"action"},
		},
	}
}

// GitPRToolDeclaration returns the declaration for the git_pr tool.
func GitPRToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "git_pr",
		Description: "Creates and manages GitHub pull requests using gh CLI. Supports auto-generating PR descriptions from commit history.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"action": {
					Type:        genai.TypeString,
					Description: "PR action: 'create', 'list', 'view', 'checks', 'merge', 'close'",
					Enum:        []string{"create", "list", "view", "checks", "merge", "close"},
				},
				"title": {
					Type:        genai.TypeString,
					Description: "PR title",
				},
				"body": {
					Type:        genai.TypeString,
					Description: "PR description",
				},
				"base": {
					Type:        genai.TypeString,
					Description: "Base branch",
				},
				"draft": {
					Type:        genai.TypeBoolean,
					Description: "Create as draft PR",
				},
				"pr_number": {
					Type:        genai.TypeString,
					Description: "PR number (for view/checks/merge/close)",
				},
				"auto_description": {
					Type:        genai.TypeBoolean,
					Description: "Auto-generate PR description from commits",
				},
			},
			Required: []string{"action"},
		},
	}
}

// PinContextToolDeclaration returns the declaration for the pin_context tool.
func PinContextToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "pin_context",
		Description: "Pins information to the system prompt for the rest of the session. Use for important context you need to remember.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"content": {
					Type:        genai.TypeString,
					Description: "Text to pin to system prompt",
				},
				"clear": {
					Type:        genai.TypeBoolean,
					Description: "If true, clear existing pinned context",
				},
			},
			Required: []string{"content"},
		},
	}
}

// HistorySearchToolDeclaration returns the declaration for the history_search tool.
func HistorySearchToolDeclaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        "history_search",
		Description: "Searches through session message history using regex. Use to recover details lost to context truncation.",
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"pattern": {
					Type:        genai.TypeString,
					Description: "Regex pattern to search for in history",
				},
			},
			Required: []string{"pattern"},
		},
	}
}
