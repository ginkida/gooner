package commands

import (
	"context"
	"fmt"
)

// QuickstartCommand provides a quick start guide.
type QuickstartCommand struct{}

const (
	quickstartHeader = `
%s╔═══════════════════════════════════════════════════════════════╗
║                    Quick Start with Gooner                    ║
╚═══════════════════════════════════════════════════════════════╝%s

Gooner is an AI assistant that understands your project context
and helps with coding using natural language.

%s──── 5 Simple Examples to Get Started ───%s
`

	quickstartExamples = `
%s1. Working with Files%s

    "Read README.md"
    "Create a config.yaml file with database settings"
    "Add logging to the ProcessRequest function"

%s2. Code Search%s

    "Find all .go files in the cmd directory"
    "Show all functions that start with Handle"
    "Find where the config variable is used"

%s3. Running Commands%s

    "Run tests in the internal/auth package"
    "Build the project and show the binary size"
    "Install dependencies"

%s4. Working with Git%s

    "Show git status"
    "Create a commit with message 'fix: resolve login issue'"
    "Show the last 5 commits"

%s5. Refactoring%s

    "Rename function oldName to newName in all files"
    "Extract validation logic into a separate function"
    "Find duplicate code"
`

	quickstartTips = `
%s──── Helpful Tips ───%s

  • %sBe specific%s           - The more detailed the request, the better the result
  • %sUse context%s           - "In main.go find the main function"
  • %sAsk for explanations%s  - "Explain how this code works"
  • %sReview changes%s        - Use /undo to roll back

%s──── Key Commands ───%s

  %s/help%s       - Help for all commands
  %s/tour%s       - Interactive tutorial
  %s/examples%s   - More example prompts
  %s/model%s      - Switch AI model
  %s/clear%s      - Clear chat history

%s──── Ready to Start? ───%s

Just start asking questions! Gooner understands natural language.

%sExample:%s "Analyze the project structure and suggest improvements"
`
)

func (c *QuickstartCommand) Name() string {
	return "quickstart"
}

func (c *QuickstartCommand) Description() string {
	return "Quick start guide with examples"
}

func (c *QuickstartCommand) Usage() string {
	return "/quickstart"
}

func (c *QuickstartCommand) GetMetadata() CommandMetadata {
	return CommandMetadata{
		Category: CategoryGettingStarted,
		Icon:     "rocket",
		Priority: 10,
	}
}

func (c *QuickstartCommand) Execute(ctx context.Context, args []string, app AppInterface) (string, error) {
	return c.getQuickstart(), nil
}

func (c *QuickstartCommand) getQuickstart() string {
	header := fmt.Sprintf(quickstartHeader, colorCyan, colorReset, colorYellow, colorReset)
	examples := fmt.Sprintf(quickstartExamples, colorCyan, colorReset, colorCyan, colorReset, colorCyan, colorReset, colorCyan, colorReset, colorCyan, colorReset)
	tips := fmt.Sprintf(quickstartTips, colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset,
		colorYellow, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset, colorGreen, colorReset,
		colorYellow, colorReset, colorCyan, colorReset)
	return header + examples + tips
}
