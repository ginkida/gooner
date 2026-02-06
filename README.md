![Gokin](https://minio.ginkida.dev/minion/github/gokin.jpg)

<p align="center">
  <a href="https://github.com/ginkida/gokin/releases"><img src="https://img.shields.io/github/v/release/ginkida/gokin" alt="Release"></a>
  <a href="https://github.com/ginkida/gokin/blob/main/LICENSE"><img src="https://img.shields.io/github/license/ginkida/gokin" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go" alt="Go Version">
</p>

<p align="center">
  <img src="https://minio.ginkida.dev/minion/github/Gokin-cli.gif" alt="Gokin Demo" width="800">
</p>

**Gokin** is an AI-powered coding assistant for your terminal — like Claude Code, but multi-provider and budget-friendly.

- **Multi-provider** — Gemini, DeepSeek, GLM, Ollama (local models)
- **Multi-agent** — specialized agents with tree-based planning
- **$1–3/month** or **free** with Ollama / Gemini free tier

---

## Why Gokin?

### Cost Comparison

| Tool | Cost | Best For |
|------|------|----------|
| Gokin + Ollama | Free (local) | Privacy-focused, offline development |
| Gokin + GLM-4 | ~$3/month | Initial development, bulk operations |
| Gokin + DeepSeek | ~$1/month | Coding tasks, great value |
| Gokin + Gemini Flash 3 | Free tier available | Fast iterations, prototyping |
| Claude Code | ~$100/month | Final polish, complex reasoning |
| Cursor | ~$20/month | IDE-integrated AI |
| Aider | API costs only | Git-focused editing |

### Recommended Workflow

```
Gokin (GLM-4 / Gemini Flash 3)   →     Claude Code (Claude Opus 4.5)
        ↓                                         ↓
   Write code from scratch              Polish and refine the code
   Bulk file operations                 Complex architectural decisions
   Repetitive tasks                     Code review and optimization
```

### Why Not Just Use Other Tools?

| Feature | Gokin | Claude Code | Cursor | Aider |
|---------|-------|-------------|--------|-------|
| Multi-provider | 4 providers + Ollama | Claude only | Multiple | Multiple |
| Terminal-native | Yes | Yes | IDE | Terminal |
| Local models | Ollama | No | No | Yes |
| Multi-agent | Yes | No | No | No |
| Tree planning | Beam/MCTS/A* | No | No | No |
| Full code control | Open source | Closed | Closed | Open source |
| Cost (monthly) | $0–3 | ~$100 | ~$20 | API costs |

## Features

### Core
- **File Operations** — Read, create, edit, copy, move, delete files and directories (including PDF, images, Jupyter notebooks)
- **Shell Execution** — Run commands with timeout, background execution, sandbox mode
- **Search** — Glob patterns, regex grep (with regex replacement in edit), semantic search with embeddings

### AI Providers
- **Google Gemini** — Gemini 3 Pro/Flash, free tier available
- **DeepSeek** — Excellent coding model, very affordable (~$1/month)
- **GLM-4** — Cost-effective Chinese model (~$3/month)
- **Ollama** — Local LLMs (Llama, Qwen, DeepSeek, CodeLlama), free & private

### Intelligence
- **Multi-Agent System** — Specialized agents (Explore, Bash, Plan, General) with adaptive delegation
- **Tree Planner** — Advanced planning with Beam Search, MCTS, A* algorithms
- **Context Predictor** — Predicts needed files based on access patterns
- **Semantic Search** — Find code by meaning, not just keywords

### Productivity
- **Git Integration** — Status, add, commit, pull request, blame, diff, log
- **Task Management** — Todo list, background tasks
- **Memory System** — Remember information between sessions
- **Sessions** — Save and restore conversation state
- **Undo/Redo** — Revert file changes (including copy, move, delete operations)

### Extensibility
- **MCP Support** — Connect to external MCP servers for additional tools
- **Custom Agent Types** — Register your own specialized agents
- **Permission System** — Control which operations require approval
- **Hooks** — Automate actions (pre/post tool, on error, on start/exit)
- **Themes** — Light and dark mode
- **GOKIN.md** — Project-specific instructions

## Installation

### Quick Install (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/ginkida/gokin/main/install.sh | sh
```

Downloads the latest release binary for your OS and architecture, installs to `~/.local/bin`.

### From Source

```bash
git clone https://github.com/ginkida/gokin.git
cd gokin
go build -o gokin ./cmd/gokin

# Or install to ~/go/bin
go install ./cmd/gokin
```

### Requirements

- Go 1.25+
- One of:
  - Google account with Gemini subscription (OAuth login)
  - Google Gemini API key (free tier available)
  - DeepSeek API key
  - GLM-4 API key
  - Ollama installed locally (no API key needed)

## Quick Start

### 1. Authentication

**OAuth (recommended for Gemini subscribers):**
```bash
gokin
> /oauth-login
```

**API Key:**
```bash
export GEMINI_API_KEY="your-api-key"  # Get free key at https://aistudio.google.com/apikey
gokin
```

### 2. Launch

```bash
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
| **DeepSeek** | deepseek-chat, deepseek-reasoner | ~$1/month | Coding tasks, reasoning |
| **GLM** | glm-4.7 | ~$3/month | Budget-friendly development |
| **Ollama** | Any model from `ollama list` | Free (local) | Privacy, offline, custom models |

### Model Presets

| Preset | Provider | Model | Use Case |
|--------|----------|-------|----------|
| `fast` | Gemini | gemini-3-flash-preview | Quick responses |
| `creative` | Gemini | gemini-3-pro-preview | Complex tasks |
| `coding` | GLM | glm-4.7 | Budget coding |

### Switching Providers

```yaml
# config.yaml
model:
  provider: "gemini"           # gemini, deepseek, glm, or ollama
  name: "gemini-3-flash-preview"
  preset: "fast"               # or use preset instead
```

Or via environment: `export GOKIN_BACKEND="gemini"`

### Using Ollama

```bash
# 1. Install Ollama (https://ollama.ai)
curl -fsSL https://ollama.ai/install.sh | sh

# 2. Pull a model
ollama pull llama3.2

# 3. Run Gokin with Ollama
gokin --model llama3.2
```

> **Note:** Tool calling support varies by model. Llama 3.1+, Qwen 2.5+, and Mistral have good tool support.

## Commands

All commands start with `/`:

| Command | Description |
|---------|-------------|
| `/help [command]` | Show help |
| `/clear` | Clear conversation history |
| `/compact` | Force context compression |
| `/cost` | Show token usage and cost |
| `/sessions` | List saved sessions |
| `/save [name]` | Save current session |
| `/resume <id>` | Restore session |
| `/undo` | Undo last file change |
| `/commit [-m message]` | Create commit |
| `/pr [--title title]` | Create pull request |
| `/config` | Show current configuration |
| `/doctor` | Check environment |
| `/init` | Create GOKIN.md for project |
| `/model <name>` | Change AI model |
| `/theme` | Switch UI theme |
| `/permissions` | Manage tool permissions |
| `/sandbox` | Toggle sandbox mode |
| `/update` | Check for and install updates |
| `/browse` | Interactive file browser |
| `/copy` / `/paste` | Clipboard operations |
| `/oauth-login` | Login via Google account |
| `/login <provider> <key>` | Set API key (gemini, deepseek, glm, ollama) |
| `/logout` | Remove saved API key |
| `/semantic-stats` | Semantic index statistics |
| `/semantic-reindex` | Force reindex |
| `/semantic-cleanup` | Clean up old projects |
| `/register-agent-type` | Register custom agent type |

## AI Tools

AI has access to 58 tools across 8 categories:

| Category | Tools | Description |
|----------|-------|-------------|
| **File Operations** | `read`, `write`, `edit`, `copy`, `move`, `delete`, `mkdir`, `diff`, `batch` | Create, modify, and manage files and directories |
| **Search & Navigation** | `glob`, `grep`, `list_dir`, `tree`, `semantic_search`, `code_graph` | Find files by pattern, content, or meaning |
| **Execution** | `bash`, `ssh`, `kill_shell`, `env` | Run commands with timeout, sandbox, background mode |
| **Git** | `git_status`, `git_add`, `git_commit`, `git_log`, `git_blame`, `git_diff` | Full git workflow |
| **Web** | `web_fetch`, `web_search` | Fetch URLs and search the internet |
| **Planning** | `enter_plan_mode`, `update_plan_progress`, `get_plan_status`, `exit_plan_mode`, `todo`, `task` | Plan and execute complex tasks |
| **Memory** | `memory`, `shared_memory`, `scratchpad`, `memorize`, `ask_user` | Persistent storage and inter-agent communication |
| **Code Analysis** | `refactor`, `pattern_search`, `code_oracle`, `check_impact`, `verify_code` | Pattern-based refactoring and impact analysis |

## Configuration

Config file: `~/.config/gokin/config.yaml`

```yaml
api:
  gemini_key: ""               # Or via GEMINI_API_KEY env var
  deepseek_key: ""             # Or via DEEPSEEK_API_KEY
  glm_key: ""                  # Or via GLM_API_KEY
  backend: "gemini"            # gemini, deepseek, glm, or ollama

model:
  name: "gemini-3-flash-preview"
  temperature: 1.0
  max_output_tokens: 8192

tools:
  timeout: 2m
  bash:
    sandbox: true

permission:
  enabled: true
  default_policy: "ask"        # allow, ask, deny
```

### Environment Variables

| Variable | Description |
|----------|-------------|
| `GEMINI_API_KEY` | Gemini API key |
| `DEEPSEEK_API_KEY` | DeepSeek API key |
| `GLM_API_KEY` | GLM API key |
| `OLLAMA_API_KEY` | Ollama API key (for remote servers) |
| `OLLAMA_HOST` | Ollama server URL (default: http://localhost:11434) |
| `GOKIN_MODEL` | Model name (overrides config) |
| `GOKIN_BACKEND` | Backend: gemini, deepseek, glm, or ollama |

### File Locations

| Path | Contents |
|------|----------|
| `~/.config/gokin/config.yaml` | Configuration |
| `~/.local/share/gokin/sessions/` | Saved sessions |
| `~/.local/share/gokin/memory/` | Memory data |
| `~/.config/gokin/semantic_cache/` | Semantic search index |

## MCP (Model Context Protocol)

Extend Gokin with external tools via [MCP servers](https://modelcontextprotocol.io/).

```yaml
# config.yaml
mcp:
  enabled: true
  servers:
    - name: github
      transport: stdio
      command: npx
      args: ["-y", "@modelcontextprotocol/server-github"]
      env:
        GITHUB_PERSONAL_ACCESS_TOKEN: "${GITHUB_TOKEN}"
      auto_connect: true
```

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

- **Automatic Secret Redaction** — API keys, tokens, passwords are masked in AI output and logs
- **Sandbox Mode** — Bash commands run in a restricted environment with blocked dangerous commands
- **Permission System** — Control which tools require approval (allow / ask / deny per tool)
- **Environment Isolation** — API keys excluded from subprocesses, config files use owner-only permissions

```
# What appears in files:              # What AI sees:
export GEMINI_API_KEY="AIzaSy..."  →  export GEMINI_API_KEY="[REDACTED]"
password: "super_secret_123"       →  password: "[REDACTED]"
```

## Advanced Features

### Multi-Agent System
Specialized agents (Explore, Bash, Plan, General) coordinate via shared memory, delegate subtasks, and self-correct errors. Register custom agent types with `/register-agent-type`.

### Planning Mode
AI creates step-by-step plans, requests approval, then executes with progress reports. Uses advanced algorithms: Beam Search, MCTS, A* for complex task decomposition.

### Semantic Search
Find code by meaning using embeddings. Project is auto-indexed on launch; search with natural language queries like "where is authentication implemented?"

### Memory System
AI remembers information between sessions. Stored in `~/.local/share/gokin/memory/`. Just say "remember that this project uses PostgreSQL 15."

### Hooks
Automate actions via shell commands on events: `pre_tool`, `post_tool`, `on_error`, `on_start`, `on_exit`. Configure in `config.yaml` under `hooks:`.

### GOKIN.md
Create project-specific instructions with `/init`. AI reads this file on startup for project context, code standards, and build commands.

### Keyboard Shortcuts

| Key | Action |
|-----|--------|
| `Enter` | Send message |
| `Ctrl+C` | Interrupt / Exit |
| `Ctrl+P` | Command palette |
| `Ctrl+G` | Toggle select mode (freeze viewport for text selection) |
| `Option+C` | Copy last AI response (macOS) |
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
