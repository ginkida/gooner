# Contributing to Gokin

Thank you for your interest in contributing to Gokin!

## Getting Started

### Prerequisites

- Go 1.23 or higher
- Git

### Setup

```bash
# Clone the repository
git clone https://github.com/ginkida/gokin.git
cd gokin

# Install dependencies
go mod download

# Build
go build -o gokin ./cmd/gokin

# Run
./gokin
```

## Development

### Project Structure

```
gokin/
├── cmd/gokin/          # Entry point
├── internal/
│   ├── app/             # Application orchestrator
│   ├── agent/           # AI agent system
│   ├── client/          # API clients (Gemini, GLM)
│   ├── tools/           # AI tools (read, write, bash, etc.)
│   ├── ui/              # TUI interface (Bubble Tea)
│   ├── config/          # Configuration
│   ├── context/         # Context management
│   ├── permission/      # Permission system
│   └── ...
├── go.mod
└── README.md
```

### Building

```bash
# Development build
go build -o gokin ./cmd/gokin

# Production build with version info
go build -ldflags "-X main.version=1.0.0 -X main.commit=$(git rev-parse --short HEAD)" -o gokin ./cmd/gokin
```

### Code Style

- Follow standard Go conventions
- Use `gofmt` for formatting
- Use `go vet` for static analysis

```bash
# Format code
gofmt -w .

# Run vet
go vet ./...
```

## Making Changes

### Workflow

1. Fork the repository
2. Create a feature branch: `git checkout -b feature/my-feature`
3. Make your changes
4. Run tests and linting
5. Commit with clear messages
6. Push to your fork
7. Open a Pull Request

### Commit Messages

Use clear, descriptive commit messages:

```
feat: add semantic search support
fix: resolve token counting issue
docs: update installation instructions
refactor: simplify context manager
```

### Pull Requests

- Describe what changes you made and why
- Reference any related issues
- Keep PRs focused on a single feature or fix

## Adding New Tools

To add a new AI tool:

1. Create a new file in `internal/tools/`
2. Implement the `Tool` interface
3. Register the tool in `internal/tools/registry.go`

Example:

```go
// internal/tools/my_tool.go
package tools

type MyTool struct{}

func (t *MyTool) Name() string {
    return "my_tool"
}

func (t *MyTool) Description() string {
    return "Description of what the tool does"
}

func (t *MyTool) Execute(ctx context.Context, params map[string]any) (string, error) {
    // Implementation
}
```

## Reporting Issues

When reporting bugs:

- Use the [issue templates](.github/ISSUE_TEMPLATE/) provided
- Use a clear, descriptive title
- Describe steps to reproduce
- Include expected vs actual behavior
- Add system info (OS, Go version, Gokin version, AI provider)
- Provide logs if possible (use `--verbose` or `--debug`)

## Code of Conduct

Please note that this project adheres to a [Code of Conduct](.github/CODE_OF_CONDUCT.md).
By participating, you are expected to uphold this code. Please report unacceptable
behavior to [ya-ginkida@yandex.kz](mailto:ya-ginkida@yandex.kz).

## Questions?

Feel free to open an issue for questions or discussions. We welcome:

- Bug reports (use the [Bug Report template](.github/ISSUE_TEMPLATE/bug_report.md))
- Feature requests (use the [Feature Request template](.github/ISSUE_TEMPLATE/feature_request.md))
- Questions (use the [Question template](.github/ISSUE_TEMPLATE/question.md))
- Documentation improvements (use the [Documentation template](.github/ISSUE_TEMPLATE/documentation.md))

## License

By contributing to Gokin, you agree that your contributions will be licensed under the same license as the project.
