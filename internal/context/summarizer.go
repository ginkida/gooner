package context

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gokin/internal/client"
	"gokin/internal/config"

	"google.golang.org/genai"
)

const summarizationPrompt = `Summarize this development conversation for context preservation.

PRIORITIES (highest to lowest):
1. Specific file paths and modified functions/methods
2. Error messages encountered and how they were resolved
3. Dependencies discovered between components
4. Configuration values set or changed
5. Key architectural decisions and their reasoning
6. Unresolved issues or next steps

DO NOT include:
- Verbose tool output or raw logs
- Intermediate failed attempts (only final solutions)
- UI confirmations or acknowledgments
- Repeated file reads of the same content

Format: Use bullet points grouped by topic. Start each group with the relevant file path.

CONVERSATION TO SUMMARIZE:
%s

SUMMARY:`

// Summarizer handles conversation summarization.
type Summarizer struct {
	client client.Client
}

// NewSummarizer creates a new summarizer.
func NewSummarizer(c client.Client) *Summarizer {
	return &Summarizer{
		client: c,
	}
}

// SetClient updates the underlying client.
func (s *Summarizer) SetClient(c client.Client) {
	s.client = c
}

// SetConfig updates the summarizer configuration.
func (s *Summarizer) SetConfig(cfg *config.ContextConfig) {
	// Currently no config-specific state in Summarizer,
	// but this keeps it consistent for future use.
}

// Summarize creates a summary of the given messages.
func (s *Summarizer) Summarize(ctx context.Context, messages []*genai.Content) (*genai.Content, error) {
	// If too many messages, summarize in chunks first to avoid prompt blowup
	if len(messages) > 100 {
		return s.summarizeHierarchical(ctx, messages)
	}

	return s.doSummarize(ctx, messages)
}

func (s *Summarizer) doSummarize(ctx context.Context, messages []*genai.Content) (*genai.Content, error) {
	// Format messages for summarization
	formatted := s.formatMessages(messages)

	// Create summarization prompt
	prompt := fmt.Sprintf(summarizationPrompt, formatted)

	// Send to model for summarization
	stream, err := s.client.SendMessage(ctx, prompt)
	if err != nil {
		return nil, fmt.Errorf("summarization request failed: %w", err)
	}

	// Collect response
	resp, err := stream.Collect()
	if err != nil {
		return nil, fmt.Errorf("summarization response failed: %w", err)
	}

	// Create summary content as a user message (context injection)
	summaryText := fmt.Sprintf("[Previous conversation summary]\n%s\n[End of summary]", resp.Text)
	summary := genai.NewContentFromText(summaryText, genai.RoleUser)

	return summary, nil
}

// summarizeHierarchical handles large conversations by splitting on semantic boundaries.
func (s *Summarizer) summarizeHierarchical(ctx context.Context, messages []*genai.Content) (*genai.Content, error) {
	chunks := s.splitOnBoundaries(messages)
	var midSummaries []string

	for _, chunk := range chunks {
		formatted := s.formatMessages(chunk)
		prompt := fmt.Sprintf("Summarize this segment of a development conversation. Focus on file changes, errors resolved, and technical decisions:\n\n%s", formatted)

		stream, err := s.client.SendMessage(ctx, prompt)
		if err != nil {
			return nil, err
		}
		resp, err := stream.Collect()
		if err != nil {
			return nil, err
		}
		midSummaries = append(midSummaries, resp.Text)
	}

	// Final summarization of summaries
	finalPrompt := fmt.Sprintf("Combine these conversation segment summaries into a single cohesive technical summary. Group by file/component:\n\n%s", strings.Join(midSummaries, "\n\n---\n\n"))
	stream, err := s.client.SendMessage(ctx, finalPrompt)
	if err != nil {
		return nil, err
	}
	resp, err := stream.Collect()
	if err != nil {
		return nil, err
	}

	summaryText := fmt.Sprintf("[Long-term conversation summary]\n%s\n[End of summary]", resp.Text)
	return genai.NewContentFromText(summaryText, genai.RoleUser), nil
}

// splitOnBoundaries splits messages into chunks at semantic boundaries
// (low-priority messages like confirmations, repeated reads, verbose logs).
func (s *Summarizer) splitOnBoundaries(messages []*genai.Content) [][]*genai.Content {
	const maxChunkSize = 60
	const minChunkSize = 15

	var chunks [][]*genai.Content
	var current []*genai.Content

	for _, msg := range messages {
		current = append(current, msg)

		// Split at boundary if chunk is large enough
		if len(current) >= minChunkSize && s.isLowPriorityMessage(msg) {
			chunks = append(chunks, current)
			current = nil
		}

		// Force split at max chunk size
		if len(current) >= maxChunkSize {
			chunks = append(chunks, current)
			current = nil
		}
	}

	// Append remaining
	if len(current) > 0 {
		if len(chunks) > 0 && len(current) < minChunkSize {
			// Merge small tail with last chunk
			chunks[len(chunks)-1] = append(chunks[len(chunks)-1], current...)
		} else {
			chunks = append(chunks, current)
		}
	}

	return chunks
}

// isLowPriorityMessage checks if a message is a natural boundary for chunking.
func (s *Summarizer) isLowPriorityMessage(msg *genai.Content) bool {
	if msg == nil {
		return false
	}
	for _, part := range msg.Parts {
		// Confirmations and acknowledgments
		if part.Text != "" {
			lower := strings.ToLower(part.Text)
			if len(lower) < 100 {
				for _, phrase := range []string{"ok", "done", "got it", "understood", "sure", "yes", "confirmed"} {
					if strings.Contains(lower, phrase) {
						return true
					}
				}
			}
		}
		// Verbose read-only tool responses
		if part.FunctionResponse != nil {
			name := part.FunctionResponse.Name
			if name == "read" || name == "glob" || name == "tree" || name == "list_dir" || name == "env" {
				return true
			}
		}
	}
	return false
}

// formatMessages formats messages for summarization prompt.
func (s *Summarizer) formatMessages(messages []*genai.Content) string {
	var builder strings.Builder

	for _, msg := range messages {
		role := "User"
		if msg.Role == genai.RoleModel {
			role = "Assistant"
		}

		for _, part := range msg.Parts {
			if part.Text != "" {
				text := part.Text
				// If it's a very large text block (likely a file or multi-file output),
				// try to keep the structure instead of just truncating the beginning.
				if len(text) > 4000 {
					lines := strings.Split(text, "\n")
					if len(lines) > 100 {
						// Keep first 30 lines, middle 10 (notified as skip), and last 30 lines
						head := strings.Join(lines[:40], "\n")
						tail := strings.Join(lines[len(lines)-40:], "\n")
						text = fmt.Sprintf("%s\n\n... [%d lines skipped] ...\n\n%s", head, len(lines)-80, tail)
					} else {
						text = text[:4000] + "... [truncated]"
					}
				}
				builder.WriteString(fmt.Sprintf("%s: %s\n\n", role, text))
			}
			if part.FunctionCall != nil {
				args, _ := json.Marshal(part.FunctionCall.Args)
				builder.WriteString(fmt.Sprintf("%s: [Called tool: %s with args: %s]\n\n", role, part.FunctionCall.Name, string(args)))
			}
			if part.FunctionResponse != nil {
				respContent := "[Tool response]"
				if content, ok := part.FunctionResponse.Response["content"].(string); ok {
					// Smarter response summarization
					if len(content) > 1000 {
						lines := strings.Split(content, "\n")
						if len(lines) > 40 {
							head := strings.Join(lines[:20], "\n")
							tail := strings.Join(lines[len(lines)-20:], "\n")
							content = fmt.Sprintf("%s\n\n... [%d lines skipped] ...\n\n%s", head, len(lines)-40, tail)
						} else {
							content = content[:1000] + "..."
						}
					}
					respContent = content
				} else if err, ok := part.FunctionResponse.Response["error"].(string); ok && err != "" {
					respContent = "Error: " + err
				}
				builder.WriteString(fmt.Sprintf("Tool (%s) Result: %s\n\n", part.FunctionResponse.Name, respContent))
			}
		}
	}

	return builder.String()
}

// SummarizeIfNeeded checks if summarization is needed and performs it.
func (s *Summarizer) SummarizeIfNeeded(ctx context.Context, messages []*genai.Content, currentTokens, maxTokens int, threshold float64) (*genai.Content, bool, error) {
	percentUsed := float64(currentTokens) / float64(maxTokens)

	if percentUsed < threshold {
		return nil, false, nil
	}

	// Need summarization
	summary, err := s.Summarize(ctx, messages)
	if err != nil {
		return nil, false, err
	}

	return summary, true, nil
}

// ExtractKeyInfo extracts key information from messages.
func (s *Summarizer) ExtractKeyInfo(messages []*genai.Content) KeyInfo {
	info := KeyInfo{
		FilesModified:  make([]string, 0),
		ToolsUsed:      make([]string, 0),
		KeyDecisions:   make([]string, 0),
		PendingActions: make([]string, 0),
	}

	toolsSeen := make(map[string]bool)

	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.FunctionCall != nil {
				name := part.FunctionCall.Name
				if !toolsSeen[name] {
					toolsSeen[name] = true
					info.ToolsUsed = append(info.ToolsUsed, name)
				}

				// Track file modifications
				if name == "write" || name == "edit" {
					if path, ok := part.FunctionCall.Args["path"].(string); ok {
						info.FilesModified = append(info.FilesModified, path)
					}
				}
			}
		}
	}

	return info
}

// DistillToolResult summarizes a large tool result using the LLM.
func (s *Summarizer) DistillToolResult(ctx context.Context, toolName string, content string) (string, error) {
	prompt := fmt.Sprintf(`Summarize the output of the tool "%s". 
The output is very long and needs to be distilled for a conversation history. 
Keep:
- Key information (e.g., specific errors, successful outcomes, important statistics)
- Crucial technical details
- A high-level overview of what the output contains

Be concise. If there are errors, make sure they are highlighted.

TOOL OUTPUT:
%s

DISTILLED SUMMARY:`, toolName, content)

	stream, err := s.client.SendMessage(ctx, prompt)
	if err != nil {
		return "", err
	}

	resp, err := stream.Collect()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf("[Distilled Output for %s]:\n%s", toolName, resp.Text), nil
}

// KeyInfo holds extracted key information from a conversation.
type KeyInfo struct {
	FilesModified  []string
	ToolsUsed      []string
	KeyDecisions   []string
	PendingActions []string
}
