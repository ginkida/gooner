package context

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gooner/internal/client"
	"gooner/internal/config"

	"google.golang.org/genai"
)

const summarizationPrompt = `Summarize this conversation for context preservation. Keep:
- Key decisions made
- Code changes and their reasons
- Unresolved issues or tasks
- Critical context needed to continue the conversation
- File paths and function names mentioned

Be concise but preserve important technical details.

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

// summarizeHierarchical handles extremely large conversations by summarizing chunks first.
func (s *Summarizer) summarizeHierarchical(ctx context.Context, messages []*genai.Content) (*genai.Content, error) {
	const chunkSize = 50
	var midSummaries []string

	for i := 0; i < len(messages); i += chunkSize {
		end := i + chunkSize
		if end > len(messages) {
			end = len(messages)
		}

		formatted := s.formatMessages(messages[i:end])
		prompt := fmt.Sprintf("Summarize this segment of a long development conversation. Focus on technical decisions and file changes:\n\n%s", formatted)

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
	finalPrompt := fmt.Sprintf("Combine these conversation segment summaries into a single cohesive technical summary:\n\n%s", strings.Join(midSummaries, "\n\n---\n\n"))
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

// KeyInfo holds extracted key information from a conversation.
type KeyInfo struct {
	FilesModified  []string
	ToolsUsed      []string
	KeyDecisions   []string
	PendingActions []string
}
