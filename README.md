![Gokin](https://minio.ginkida.dev/minion/github/gokin.jpg)

<p align="center">
  <a href="https://github.com/ginkida/gokin/releases"><img src="https://img.shields.io/github/v/release/ginkida/gokin" alt="Release"></a>
  <a href="https://github.com/ginkida/gokin/blob/main/LICENSE"><img src="https://img.shields.io/github/license/ginkida/gokin" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go" alt="Go Version">
</p>

<p align="center">
  <img src="https://minio.ginkida.dev/minion/github/Gokin-cli.gif" alt="Gokin Demo" width="800">
</p>

---

## Why Gokin?

I created Gokin as a companion to [Claude Code](https://github.com/anthropics/claude-code). When my Claude Code limits ran out, I needed a tool that could:

- **Write projects from scratch** — Gokin handles the heavy lifting of initial development
- **Save money** — GLM-4 costs ~$3/month vs Claude Code's ~$100/month
- **Stay secure** — I don't trust Chinese AI company CLIs with my code, so I built my own

### Recommended Workflow

```
Gokin (GLM-4 / Gemini Flash 3)   →     Claude Code (Claude Opus 4.5)
        ↓                                         ↓
   Write code from scratch              Polish and refine the code
   Bulk file operations                 Complex architectural decisions
   Repetitive tasks                     Code review and optimization
```

### Cost Comparison

| Tool | Cost | Best For |
|------|------|----------|
| Gokin + Ollama | Free (local) | Privacy-focused, offline development |
| Gokin + GLM-4 | ~$3/month | Initial development, bulk operations |
| Gokin + Gemini Flash 3 | Free tier available | Fast iterations, prototyping |
| Claude Code | ~$100/month | Final polish, complex reasoning |

> **Note:** Chinese models are currently behind frontier models like Claude, but they're improving rapidly. For best performance, use Gokin with **Gemini Flash 3** — it's fast, capable, and has a generous free tier.

## Features

### Core
- **File Operations** — Read, create, edit files (including PDF, images, Jupyter notebooks)
- **Shell Execution** — Run commands with timeout, background execution, sandbox mode
- **Search** — Glob patterns, regex grep, semantic search with embeddings

### AI Providers
- **Google Gemini** — Gemini 3 Pro/Flash, free tier available
- **GLM-4** — Cost-effective Chinese model (~$3/month)
- **Ollama** — Local LLMs (Llama, Qwen, DeepSeek, CodeLlama), free & private

### Intelligence
- **Multi-Agent System** — Specialized agents (Explore, Bash, Plan, General)
- **Tree Planner** — Advanced planning with Beam Search, MCTS, A* algorithms
- **Semantic Search** — Find code by meaning, not just keywords

### Productivity
- **Git Integration** — Commit, pull request, blame, diff, log
- **Task Management** — Todo list, background tasks
- **Memory System** — Remember information between sessions
- **Sessions** — Save and restore conversation state
- **Undo/Redo** — Revert file changes

### Extensibility
- **MCP Support** — Connect to external MCP servers for additional tools
- **Permission System** — Control which operations require approval
- **Hooks** — Automate actions (pre/post tool, on error, on start/exit)
- **Themes** — Light and dark mode
- **GOKIN.md** — Project-specific instructions

## Installation

```bash
# Clone the repository
git clone https://github.com/ginkida/gokin.git
cd gokin

# Build
go build -o gokin ./cmd/gokin

# Install via Go (recommended for macOS/Linux)
# This installs the binary to ~/go/bin
go install ./cmd/gokin

# Make sure ~/go/bin is in your PATH:
# echo 'export PATH=$PATH:$(go env GOPATH)/bin' >> ~/.zshrc
# source ~/.zshrc

# Install to system PATH (optional)
sudo mv gokin /usr/local/bin/
```

### Requirements

- Go 1.23+
- One of:
  - Google Gemini API key (free tier available)
  - GLM-4 API key
  - Ollama installed locally (no API key needed)

## Quick Start

### 1. Get API Key

Get your free Gemini API key at: https://aistudio.google.com/apikey

### 2. Set API Key

```bash
# Via environment variable (recommended)
export GEMINI_API_KEY="your-api-key"

# Or via command in the app
gokin
> /login your-api-key
```

### 2. Launch

```bash
# In project directory
cd /path/to/your/project
gokin
```

### 3. Getting Started

```
> Hello! Tell me about this project's structure

> Find all files with .go extension

> Create a function to validate email
```

## Supported AI Providers

| Provider | Models | Cost | Best For |
|----------|--------|------|----------|
| **Gemini** | gemini-3-flash-preview, gemini-3-pro-preview | Free tier + paid | Fast iterations, prototyping |
| **GLM** | glm-4.7 | ~$3/month | Budget-friendly development |
| **Ollama** | Any model from `ollama list` | Free (local) | Privacy, offline, custom models |

### Model Presets

| Preset | Provider | Model | Use Case |
|--------|----------|-------|----------|
| `fast` | Gemini | gemini-3-flash-preview | Quick responses |
| `creative` | Gemini | gemini-3-pro-preview | Complex tasks |
| `coding` | GLM | glm-4.7 | Budget coding |

### Switching Providers

```bash
# Via environment
export GOKIN_BACKEND="gemini"   # or "glm" or "ollama"

# Via config.yaml
model:
  provider: "gemini"
  name: "gemini-3-flash-preview"
  preset: "fast"  # or use preset instead

# For Ollama (local)
model:
  provider: "ollama"
  name: "llama3.2"  # Use exact name from 'ollama list'
```

### Using Ollama

Ollama allows you to run LLMs locally without any API keys or internet connection.

```bash
# 1. Install Ollama (https://ollama.ai)
curl -fsSL https://ollama.ai/install.sh | sh

# 2. Start Ollama server
ollama serve

# 3. Pull a model (see https://ollama.ai/library for available models)
ollama pull llama3.2          # Meta's Llama 3.2
ollama pull qwen2.5-coder     # Alibaba's coding model
ollama pull deepseek-coder-v2 # DeepSeek coding model
ollama pull codellama         # Meta's Code Llama
ollama pull mistral           # Mistral 7B

# 4. List installed models
ollama list

# 5. Run Gokin with Ollama
gokin --model llama3.2
# or set in config.yaml
```

#### Finding Models

Use the exact model name from `ollama list`:

```bash
$ ollama list
NAME                    SIZE
llama3.2:latest         2.0 GB
qwen2.5-coder:7b        4.7 GB
deepseek-coder-v2:16b   8.9 GB
```

Then use it in Gokin:
```bash
gokin --model llama3.2
gokin --model qwen2.5-coder:7b
gokin --model deepseek-coder-v2:16b
```

> **Note:** Tool calling support varies by model. Models like Llama 3.1+, Qwen 2.5+, and Mistral have good tool support.

#### Remote Ollama Server

For remote Ollama servers (e.g., on a GPU server):

```yaml
# config.yaml
api:
  ollama_base_url: "http://gpu-server:11434"
  ollama_key: "optional-api-key"  # If server requires auth
```

Or via environment:
```bash
export OLLAMA_HOST="http://gpu-server:11434"
export OLLAMA_API_KEY="optional-api-key"
```

#### Ollama Cloud

Use [Ollama Cloud](https://docs.ollama.com/cloud) to run models without local GPU:

```bash
# 1. Sign in to Ollama Cloud
ollama signin

# 2. Set your API key
export OLLAMA_API_KEY="your_api_key"

# 3. Run Gokin with cloud endpoint
gokin --model llama3.2
```

Or configure in `config.yaml`:
```yaml
api:
  ollama_base_url: "https://ollama.com"
  ollama_key: "your_api_key"
model:
  provider: ollama
  name: "llama3.2"
```

> **Note:** Ollama Cloud offloads processing to cloud servers — no local GPU required.

## Commands

All commands start with `/`:

### Sessions

| Command | Description |
|---------|-------------|
| `/help [command]` | Show help |
| `/clear` | Clear conversation history |
| `/sessions` | List saved sessions |
| `/save [name]` | Save current session |
| `/resume <id>` | Restore session |

### Context and Optimization

| Command | Description |
|---------|-------------|
| `/compact` | Force context compression |
| `/cost` | Show token usage and cost |

### Semantic Search

| Command | Description |
|---------|-------------|
| `/semantic-stats` | Show index statistics |
| `/semantic-reindex` | Force reindex |
| `/semantic-cleanup` | Clean up old projects |

### Files and History

| Command | Description |
|---------|-------------|
| `/undo` | Undo last file change |

### Git

| Command | Description |
|---------|-------------|
| `/commit [-m message]` | Create commit |
| `/pr [--title title]` | Create pull request |

### Configuration

| Command | Description |
|---------|-------------|
| `/config` | Show current configuration |
| `/doctor` | Check environment |
| `/init` | Create GOKIN.md for project |
| `/model <name>` | Change AI model |
| `/theme` | Switch UI theme |
| `/permissions` | Manage tool permissions |
| `/sandbox` | Toggle sandbox mode |

### Authentication

| Command | Description |
|---------|-------------|
| `/login <api_key>` | Set Gemini API key |
| `/login <api_key> --glm` | Set GLM API key |
| `/login <api_key> --ollama` | Set Ollama API key (for remote servers) |
| `/logout` | Remove saved API key |

> **Note:** Ollama running locally doesn't require an API key. The `--ollama` flag is only needed for remote Ollama servers with authentication.

### Interface

| Command | Description |
|---------|-------------|
| `/browse` | Interactive file browser |
| `/copy` | Copy to clipboard |
| `/paste` | Paste from clipboard |
| `/stats` | Project statistics |

## AI Tools

AI has access to 40+ tools:

### File Operations

| Tool | Description |
|------|-------------|
| `read` | Read files (text, images, PDF, Jupyter notebooks) |
| `write` | Create and overwrite files |
| `edit` | Find and replace text in files |
| `diff` | Compare files |
| `batch` | Bulk operations (replace, rename, delete) on multiple files |

### Search and Navigation

| Tool | Description |
|------|-------------|
| `glob` | Search files by pattern (with .gitignore support) |
| `grep` | Search content with regex |
| `list_dir` | Directory contents |
| `tree` | Tree structure |
| `semantic_search` | Find code by meaning using embeddings |
| `code_graph` | Analyze code dependencies and imports |

### Command Execution

| Tool | Description |
|------|-------------|
| `bash` | Execute shell commands (timeout, background, sandbox) |
| `ssh` | Execute commands on remote servers |
| `kill_shell` | Stop background tasks |
| `env` | Access environment variables |

### Git Operations

| Tool | Description |
|------|-------------|
| `git_log` | Commit history |
| `git_blame` | Line-by-line authorship |
| `git_diff` | Diff between branches/commits |

### Web

| Tool | Description |
|------|-------------|
| `web_fetch` | Fetch URL content |
| `web_search` | Search the internet |

### Planning

| Tool | Description |
|------|-------------|
| `enter_plan_mode` | Start planning mode |
| `update_plan_progress` | Update plan step status |
| `verify_plan` | Verify plan against contract |
| `undo_plan` / `redo_plan` | Undo/redo plan steps |

### Task Management

| Tool | Description |
|------|-------------|
| `todo` | Create and manage task list |
| `task` | Background task management |
| `task_output` | Get background task results |

### Memory and State

| Tool | Description |
|------|-------------|
| `memory` | Persistent storage (remember/recall/forget) |
| `ask_user` | Ask user questions with options |

### Code Analysis

| Tool | Description |
|------|-------------|
| `refactor` | Pattern-based code refactoring |
| `pattern_search` | Search code patterns |

## Configuration

Configuration is stored in `~/.config/gokin/config.yaml`:

```yaml
api:
  gemini_key: ""                 # Gemini API key (or via GEMINI_API_KEY)
  glm_key: ""                    # GLM API key (or via GLM_API_KEY)
  ollama_key: ""                 # Ollama API key (optional, for remote servers)
  ollama_base_url: ""            # Ollama server URL (default: http://localhost:11434)
  backend: "gemini"              # gemini, glm, or ollama

model:
  name: "gemini-3-flash-preview" # Model name
  provider: "gemini"             # Provider: gemini or glm
  temperature: 1.0               # Temperature (0.0 - 2.0)
  max_output_tokens: 8192        # Max tokens in response
  custom_base_url: ""            # Custom API endpoint (for GLM)

tools:
  timeout: 2m                    # Tool execution timeout
  bash:
    sandbox: true                # Sandbox for commands
    blocked_commands:            # Blocked commands
      - "rm -rf /"
      - "mkfs"

ui:
  stream_output: true            # Streaming output
  markdown_rendering: true       # Markdown rendering
  show_tool_calls: true          # Show tool calls
  show_token_usage: true         # Show token usage

context:
  max_input_tokens: 0            # Input token limit (0 = default)
  warning_threshold: 0.8         # Warning threshold (80%)
  summarization_ratio: 0.5       # Compress to 50%
  tool_result_max_chars: 10000   # Max result characters
  enable_auto_summary: true      # Auto-summarization

permission:
  enabled: true                  # Permission system
  default_policy: "ask"          # allow, ask, deny
  rules:                         # Tool rules
    read: "allow"
    write: "ask"
    bash: "ask"

plan:
  enabled: true                  # Planning mode
  require_approval: true         # Require approval

hooks:
  enabled: false                 # Hooks system
  hooks: []                      # Hook list

memory:
  enabled: true                  # Memory system
  max_entries: 1000              # Max entries
  auto_inject: true              # Auto-inject into prompt

# Semantic search
semantic:
  enabled: true                  # Enable semantic search
  index_on_start: true           # Auto-index on start
  chunk_size: 500                # Characters per chunk
  chunk_overlap: 50              # Overlap between chunks
  max_file_size: 1048576         # Max file size (1MB)
  cache_dir: "~/.config/gokin/semantic_cache"  # Index cache
  cache_ttl: 168h                # Cache TTL (7 days)
  auto_cleanup: true             # Auto-cleanup old projects (>30 days)
  index_patterns:                # Indexed files
    - "*.go"
    - "*.md"
    - "*.yaml"
    - "*.yml"
    - "*.json"
    - "*.ts"
    - "*.tsx"
    - "*.js"
    - "*.py"
  exclude_patterns:              # Excluded files
    - "vendor/"
    - "node_modules/"
    - ".git/"
    - "*.min.js"
    - "*.min.css"

logging:
  level: "info"                  # debug, info, warn, error
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `GEMINI_API_KEY` | Gemini API key |
| `GOKIN_GEMINI_KEY` | Gemini API key (alternative) |
| `GLM_API_KEY` | GLM API key |
| `GOKIN_GLM_KEY` | GLM API key (alternative) |
| `OLLAMA_API_KEY` | Ollama API key (for remote servers) |
| `GOKIN_OLLAMA_KEY` | Ollama API key (alternative) |
| `OLLAMA_HOST` | Ollama server URL (default: http://localhost:11434) |
| `GOKIN_MODEL` | Model name (overrides config) |
| `GOKIN_BACKEND` | Backend: gemini, glm, or ollama |

### Secure API Key Storage

**Recommended:** Use environment variables instead of config.yaml.

```bash
# Add to ~/.bashrc or ~/.zshrc
export GEMINI_API_KEY="your-api-key"

# Or for GLM
export GLM_API_KEY="your-api-key"
```

## MCP (Model Context Protocol)

Gokin supports [MCP](https://modelcontextprotocol.io/) — a protocol for connecting AI assistants to external tools and data sources. This allows you to extend Gokin with tools from MCP servers.

### Configuration

Add MCP servers to `~/.config/gokin/config.yaml`:

```yaml
mcp:
  enabled: true
  servers:
    # GitHub integration
    - name: github
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-github"]
      env:
        GITHUB_PERSONAL_ACCESS_TOKEN: "${GITHUB_TOKEN}"
      auto_connect: true
      timeout: 30s

    # Filesystem access
    - name: filesystem
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-filesystem", "/path/to/allowed/dir"]
      auto_connect: true

    # Brave Search
    - name: brave-search
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-brave-search"]
      env:
        BRAVE_API_KEY: "${BRAVE_API_KEY}"
      auto_connect: true
```

### Server Configuration Options

| Option | Description |
|--------|-------------|
| `name` | Unique server identifier |
| `transport` | Transport type: `stdio` or `http` |
| `command` | Command to start the server (for stdio) |
| `args` | Command arguments |
| `env` | Environment variables (supports `${VAR}` expansion) |
| `url` | Server URL (for http transport) |
| `auto_connect` | Connect automatically on startup |
| `timeout` | Request timeout |
| `tool_prefix` | Prefix for tool names (default: server name) |

### How It Works

1. **Startup**: Gokin connects to configured MCP servers
2. **Tool Discovery**: Server tools are registered as Gokin tools
3. **Execution**: When AI uses an MCP tool, Gokin forwards the call to the server
4. **Response**: Results are returned to AI

### Security

- **Environment Isolation**: MCP servers run with sanitized environment (no API keys leaked)
- **Secret Expansion**: Use `${VAR}` syntax to inject secrets from environment
- **Permission System**: MCP tools go through Gokin's permission system

### Popular MCP Servers

| Server | Package | Description |
|--------|---------|-------------|
| GitHub | `@modelcontextprotocol/server-github` | GitHub API integration |
| Filesystem | `@modelcontextprotocol/server-filesystem` | File system access |
| Brave Search | `@modelcontextprotocol/server-brave-search` | Web search |
| Puppeteer | `@modelcontextprotocol/server-puppeteer` | Browser automation |
| Slack | `@modelcontextprotocol/server-slack` | Slack integration |

Find more servers at: https://github.com/modelcontextprotocol/servers

## Security

One of the main reasons I built Gokin was **security**. I don't trust Chinese AI company CLIs with access to my codebase. With Gokin, you control everything locally.

### Automatic Secret Redaction

Gokin automatically masks sensitive information in logs and AI outputs:

```
# What you type or what appears in files:
export GEMINI_API_KEY="AIzaSyD1234567890abcdefghijk"
password: "super_secret_password_123"
Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9...

# What Gokin shows to AI and logs:
export GEMINI_API_KEY="[REDACTED]"
password: "[REDACTED]"
Authorization: Bearer [REDACTED]
```

### Protected Secret Types

| Type | Pattern Example |
|------|-----------------|
| API Keys | `api_key: sk-1234...`, `GEMINI_API_KEY=AIza...` |
| AWS Credentials | `AKIA...`, `aws_secret_key=...` |
| GitHub Tokens | `ghp_...`, `gho_...`, `ghu_...` |
| Stripe Keys | `sk_live_...`, `sk_test_...` |
| JWT Tokens | `eyJhbG...` |
| Database URLs | `postgres://user:password@host` |
| Private Keys | `-----BEGIN RSA PRIVATE KEY-----` |
| Slack/Discord | Webhook URLs and bot tokens |
| Bearer Tokens | `Authorization: Bearer ...` |

### Key Masking for Display

When showing API keys in status or logs, Gokin masks the middle:

```go
// Input:  "sk-1234567890abcdef"
// Output: "sk-1****cdef"
```

### Environment Isolation

- Bash commands run with sanitized environment
- API keys are excluded from subprocesses
- Dangerous commands are blocked by default

### Model Override

```bash
export GOKIN_MODEL="gemini-3-flash-preview"
export GOKIN_BACKEND="gemini"  # or "glm"
```

## GOKIN.md

Create a `GOKIN.md` file in the project root for context:

```bash
/init
```

Example content:

```markdown
# Project Instructions for Gokin

## Project Overview
This is a Go web application using Gin framework.

## Structure
- `cmd/` - entry points
- `internal/` - internal packages
- `api/` - HTTP handlers

## Code Standards
- Use gofmt
- Comments in English
- Note: tests are not used in this project
go build -o app ./cmd/app
```

## Permission System

Default settings:

| Policy | Tools |
|--------|-------|
| `allow` | read, glob, grep, tree, diff, env, list_dir, todo, web_fetch, web_search |
| `ask` | write, edit, bash |

When permission is requested, available options:
- **Allow** — allow once
- **Allow for session** — allow until session ends
- **Deny** — deny once
- **Deny for session** — deny until session ends

## Memory System

AI can remember information between sessions:

```
> Remember that this project uses PostgreSQL 15

> What database do we use?
```

Memory is stored in `~/.local/share/gokin/memory/`.

## Semantic Search

Gokin supports semantic code search using embeddings. This allows finding code that is conceptually similar to the query, even if exact words don't match.

### How It Works

1. **Indexing**: Project is indexed on first launch
2. **Chunking**: Files are split into parts (chunks)
3. **Embeddings**: Each chunk gets a vector representation
4. **Caching**: Index is saved to `~/.config/gokin/semantic_cache/`
5. **Search**: Most similar chunks are found for queries

### Per-Project Storage

Each project is stored separately:
```
~/.config/gokin/semantic_cache/
├── a1b2c3d4e5f6g7h8/              # Project ID (SHA256 of path)
│   ├── embeddings.gob              # Embeddings cache
│   ├── index.json                 # Index metadata
│   └── metadata.json              # Project info
```

### Usage

```
> Find functions for JWT token validation

> Where is user authorization implemented?

> Show all code related to payments
```

### Management Commands

| Command | Description |
|---------|-------------|
| `/semantic-stats` | Index statistics (files, chunks, size) |
| `/semantic-reindex` | Force reindexing |
| `/semantic-cleanup` | Clean up old projects |

### Tools

**`semantic_search`** — semantic search
```json
{
  "query": "how are API errors handled",
  "top_k": 10  // number of results
}
```

**`semantic_cleanup`** — cache management
```json
{
  "action": "list",           // show all projects
  "action": "clean",          // remove old (>30 days)
  "action": "remove",         // remove specific project
  "project_id": "a1b2c3d4",
  "older_than_days": 30
}
```

### Configuration

In `config.yaml`:
```yaml
semantic:
  enabled: true                # Enable feature
  index_on_start: true         # Index on start
  chunk_size: 500              # Chunk size (characters)
  cache_ttl: 168h              # Cache TTL (7 days)
  auto_cleanup: true           # Auto-cleanup old projects
  index_patterns:              # What to index
    - "*.go"
    - "*.md"
  exclude_patterns:            # What to exclude
    - "vendor/"
    - "node_modules/"
```

### Usage Examples

**Concept search:**
```
> Where does error logging happen?

> Find code for sending email notifications

> Show all functions for database operations
```

**Combined search:**
```
> Find tests for authenticateUser function

> Show all Gin middleware
```

## Hooks

Automation via shell commands:

```yaml
hooks:
  enabled: true
  hooks:
    - name: "Log writes"
      type: "post_tool"
      tool_name: "write"
      command: "echo 'File written: ${WORK_DIR}' >> /tmp/gokin.log"
      enabled: true

    - name: "Format on save"
      type: "post_tool"
      tool_name: "write"
      command: "gofmt -w ${WORK_DIR}/*.go 2>/dev/null || true"
      enabled: true
```

Hook types:
- `pre_tool` — before execution
- `post_tool` — after successful execution
- `on_error` — on error
- `on_start` — on session start
- `on_exit` — on exit

## Planning Mode

AI can create plans and request approval:

1. AI analyzes the task
2. Creates plan with steps
3. Shows plan to user
4. Waits for approval
5. Executes step by step with reports

### Tree Planner

For complex tasks, Gokin uses advanced planning algorithms:

| Algorithm | Description |
|-----------|-------------|
| **Beam Search** | Explores multiple paths, keeps best candidates |
| **MCTS** | Monte Carlo Tree Search for exploration/exploitation |
| **A*** | Heuristic-based optimal path finding |

## Multi-Agent System

Gokin uses specialized agents for different tasks:

| Agent | Purpose |
|-------|---------|
| **ExploreAgent** | Codebase exploration and structure analysis |
| **BashAgent** | Command execution specialist |
| **PlanAgent** | Task planning and decomposition |
| **GeneralAgent** | General-purpose tasks |

Agents can:
- Coordinate with each other via messenger
- Share memory between sessions
- Delegate subtasks to specialized agents
- Self-reflect and correct errors

## Background Tasks

Long commands can run in background:

```
> Run make build in background

> Check task status
```

## File Locations

| Path | Contents |
|------|----------|
| `~/.config/gokin/config.yaml` | Configuration |
| `~/.local/share/gokin/sessions/` | Saved sessions |
| `~/.local/share/gokin/memory/` | Memory data |

## Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Enter` | Send message |
| `Ctrl+C` | Interrupt operation / Exit |
| `Ctrl+P` | Open command palette |
| `↑` / `↓` | Input history |
| `Tab` | Autocomplete |

## Usage Examples

### Code Analysis

```
> Explain what the ProcessOrder function does in order.go

> Find all places where this function is used

> Are there potential performance issues?
```

### Refactoring

```
> Rename getUserData function to fetchUserProfile in all files

> Extract repeated error handling code into a separate function
```

### Writing Code

```
> Create a REST API endpoint to get user list

> Add input validation

> Write unit tests for this endpoint
```

### Git Workflow

```
> Show changes since last commit

> /commit -m "feat: add user validation"

> /pr --title "Add user validation feature"
```

### Debugging

```
> The app crashes on startup, here's the error: [error]

> Check logs and find the cause

> Fix the problem
```

## Project Structure

```
gokin/
├── cmd/gokin/              # Entry point
├── internal/
│   ├── app/                 # Application orchestrator
│   ├── agent/               # Multi-agent system
│   │   ├── agent.go         # Base agent
│   │   ├── tree_planner.go  # Tree planning (Beam, MCTS, A*)
│   │   ├── coordinator.go   # Agent coordination
│   │   ├── reflection.go    # Self-correction
│   │   └── shared_memory.go # Inter-agent memory
│   ├── client/              # AI providers
│   │   ├── gemini.go        # Google Gemini
│   │   ├── anthropic.go     # GLM-4 (Anthropic-compatible)
│   │   └── ollama.go        # Ollama (local LLMs)
│   ├── mcp/                 # MCP (Model Context Protocol)
│   │   ├── client.go        # MCP client
│   │   ├── transport.go     # Stdio/HTTP transports
│   │   ├── manager.go       # Multi-server management
│   │   └── tool.go          # MCP tool wrapper
│   ├── tools/               # 40+ AI tools
│   │   ├── read.go, write.go, edit.go
│   │   ├── bash.go, grep.go, glob.go
│   │   ├── git_*.go         # Git operations
│   │   ├── semantic_*.go    # Semantic search
│   │   ├── plan_mode.go     # Planning tools
│   │   └── ...
│   ├── commands/            # Slash commands
│   ├── context/             # Context management & compression
│   ├── security/            # Secret redaction, path validation
│   ├── permission/          # Permission system
│   ├── hooks/               # Automation hooks
│   ├── memory/              # Persistent memory
│   ├── semantic/            # Embeddings & search
│   ├── ui/                  # TUI (Bubble Tea)
│   │   ├── tui.go           # Main model
│   │   ├── themes.go        # Light/dark themes
│   │   └── ...
│   └── config/              # Configuration
├── go.mod
└── README.md
```

## Troubleshooting

### Check Environment

```
/doctor
```

### Authentication Error

```
/auth-status
/logout
/login --oauth --client-id=YOUR_ID
```

### Context Overflow

```
/compact
```

or

```
/clear
```

### Permission Issues

Check `~/.config/gokin/config.yaml`:

```yaml
permission:
  enabled: true
  default_policy: "ask"
```

## License

MIT License
