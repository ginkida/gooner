package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gokin/internal/logging"
)

// Message represents a message exchanged between agents.
type Message struct {
	ID        string         `json:"id"`
	From      string         `json:"from"`       // Sender agent ID
	To        string         `json:"to"`         // Target role (explore, bash, etc.) or agent ID
	Type      string         `json:"type"`       // help_request, response, delegate, etc.
	Content   string         `json:"content"`    // The message text
	Data      map[string]any `json:"data"`       // Additional structured data
	Timestamp time.Time      `json:"timestamp"`
}

// AgentMessenger enables communication between agents.
// It implements the tools.Messenger interface.
type AgentMessenger struct {
	runner     *Runner
	fromAgentID string

	// Message tracking
	inbox   map[string]chan Message // agentID -> incoming messages
	pending map[string]chan string  // messageID -> response channel
	msgCounter int

	mu sync.RWMutex
}

// NewAgentMessenger creates a messenger for an agent.
func NewAgentMessenger(runner *Runner, fromAgentID string) *AgentMessenger {
	return &AgentMessenger{
		runner:     runner,
		fromAgentID: fromAgentID,
		inbox:      make(map[string]chan Message),
		pending:    make(map[string]chan string),
	}
}

// SendMessage sends a message to another agent (by role or ID).
// Returns the message ID for tracking responses.
func (m *AgentMessenger) SendMessage(msgType string, toRole string, content string, data map[string]any) (string, error) {
	m.mu.Lock()
	m.msgCounter++
	msgID := fmt.Sprintf("msg_%s_%d", m.fromAgentID, m.msgCounter)

	// Create response channel for this message
	responseChan := make(chan string, 1)
	m.pending[msgID] = responseChan
	m.mu.Unlock()

	msg := Message{
		ID:        msgID,
		From:      m.fromAgentID,
		To:        toRole,
		Type:      msgType,
		Content:   content,
		Data:      data,
		Timestamp: time.Now(),
	}

	logging.Debug("sending inter-agent message",
		"msg_id", msgID,
		"from", m.fromAgentID,
		"to", toRole,
		"type", msgType)

	// Handle the message based on type
	switch msgType {
	case "help_request":
		// Spawn a sub-agent to handle the request
		go m.handleHelpRequest(msg)
	case "delegate":
		// Delegate task to sub-agent
		go m.handleDelegation(msg)
	default:
		return "", fmt.Errorf("unknown message type: %s", msgType)
	}

	return msgID, nil
}

// ReceiveResponse waits for a response to a specific message.
func (m *AgentMessenger) ReceiveResponse(ctx context.Context, messageID string) (string, error) {
	m.mu.RLock()
	responseChan, ok := m.pending[messageID]
	m.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("no pending message with ID: %s", messageID)
	}

	select {
	case response := <-responseChan:
		// Clean up
		m.mu.Lock()
		delete(m.pending, messageID)
		m.mu.Unlock()
		return response, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(5 * time.Minute):
		// Cleanup on timeout to prevent goroutine leak
		m.mu.Lock()
		delete(m.pending, messageID)
		m.mu.Unlock()
		return "", fmt.Errorf("timeout waiting for response to message %s", messageID)
	}
}

// handleHelpRequest spawns a sub-agent to answer a help request.
func (m *AgentMessenger) handleHelpRequest(msg Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Map role to agent type
	agentType := msg.To

	// Build prompt for the helper agent
	prompt := fmt.Sprintf(
		"Another agent (ID: %s) is asking for help:\n\n%s\n\n"+
		"Please provide a helpful response to assist them.",
		msg.From, msg.Content)

	logging.Info("spawning helper agent",
		"agent_type", agentType,
		"for_message", msg.ID,
		"requester", msg.From)

	// Spawn the helper agent
	_, err := m.runner.Spawn(ctx, agentType, prompt, 15, "")

	var response string
	if err != nil {
		response = fmt.Sprintf("Error from %s agent: %v", agentType, err)
	} else {
		// Get the result
		result, ok := m.getLatestResultForType(agentType)
		if ok && result.Output != "" {
			response = result.Output
		} else {
			response = fmt.Sprintf("No response from %s agent", agentType)
		}
	}

	// Send response back (non-blocking to prevent goroutine leak)
	m.mu.RLock()
	responseChan, ok := m.pending[msg.ID]
	m.mu.RUnlock()

	if ok {
		select {
		case responseChan <- response:
			// Response sent successfully
		default:
			// Channel full or closed - response already timed out
			logging.Debug("response channel full, receiver likely timed out", "msg_id", msg.ID)
		}
	}
}

// handleDelegation spawns a sub-agent to handle a delegated task.
func (m *AgentMessenger) handleDelegation(msg Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	agentType := msg.To

	// Extract options from data
	maxTurns := 30
	model := ""
	delegationDepth := 0
	if msg.Data != nil {
		if mt, ok := msg.Data["max_turns"].(int); ok {
			maxTurns = mt
		}
		if mdl, ok := msg.Data["model"].(string); ok {
			model = mdl
		}
		if depth, ok := msg.Data["delegation_depth"].(int); ok {
			delegationDepth = depth
		}
	}

	// Increment delegation depth for the spawned agent
	delegationDepth++

	// Check if we've exceeded the maximum delegation depth
	if delegationDepth > MaxDelegationDepth {
		logging.Warn("delegation depth exceeded",
			"depth", delegationDepth,
			"max", MaxDelegationDepth,
			"from", msg.From)

		m.mu.RLock()
		responseChan, ok := m.pending[msg.ID]
		m.mu.RUnlock()

		if ok {
			responseChan <- fmt.Sprintf("Delegation failed: maximum depth (%d) exceeded", MaxDelegationDepth)
		}
		return
	}

	logging.Info("delegating to sub-agent",
		"agent_type", agentType,
		"from", msg.From,
		"msg_id", msg.ID)

	// Spawn the delegate agent
	agentID, err := m.runner.Spawn(ctx, agentType, msg.Content, maxTurns, model)

	var response string
	if err != nil {
		response = fmt.Sprintf("Delegation failed: %v", err)
	} else {
		// Get the result
		result, ok := m.runner.GetResult(agentID)
		if ok && result.Output != "" {
			response = result.Output
		} else if ok && result.Error != "" {
			response = fmt.Sprintf("Delegated agent failed: %s", result.Error)
		} else {
			response = "Delegated task completed (no output)"
		}
	}

	// Send response back (non-blocking to prevent goroutine leak)
	m.mu.RLock()
	responseChan, ok := m.pending[msg.ID]
	m.mu.RUnlock()

	if ok {
		select {
		case responseChan <- response:
			// Response sent successfully
		default:
			// Channel full or closed - response already timed out
			logging.Debug("response channel full, receiver likely timed out", "msg_id", msg.ID)
		}
	}
}

// getLatestResultForType finds the most recent result for an agent type.
func (m *AgentMessenger) getLatestResultForType(agentType string) (*AgentResult, bool) {
	at := ParseAgentType(agentType)

	m.runner.mu.RLock()
	defer m.runner.mu.RUnlock()

	var latest *AgentResult
	for _, result := range m.runner.results {
		if result.Type == at && result.Completed {
			if latest == nil || result.Duration > 0 {
				latest = result
			}
		}
	}

	return latest, latest != nil
}

// Broadcast sends a message to all agents of a given type.
func (m *AgentMessenger) Broadcast(msgType string, targetType string, content string) error {
	m.runner.mu.RLock()
	defer m.runner.mu.RUnlock()

	at := ParseAgentType(targetType)
	count := 0

	for _, agent := range m.runner.agents {
		if agent.Type == at && agent.GetStatus() == AgentStatusRunning {
			// Deliver to inbox
			m.mu.Lock()
			if inbox, ok := m.inbox[agent.ID]; ok {
				msg := Message{
					ID:        fmt.Sprintf("broadcast_%d", m.msgCounter),
					From:      m.fromAgentID,
					To:        agent.ID,
					Type:      msgType,
					Content:   content,
					Timestamp: time.Now(),
				}
				m.msgCounter++
				select {
				case inbox <- msg:
					count++
				default:
					// Inbox full, skip
				}
			}
			m.mu.Unlock()
		}
	}

	logging.Debug("broadcast sent", "type", targetType, "recipients", count)
	return nil
}

// RegisterInbox creates an inbox for an agent to receive messages.
func (m *AgentMessenger) RegisterInbox(agentID string) <-chan Message {
	m.mu.Lock()
	defer m.mu.Unlock()

	inbox := make(chan Message, 10)
	m.inbox[agentID] = inbox
	return inbox
}

// UnregisterInbox removes an agent's inbox.
func (m *AgentMessenger) UnregisterInbox(agentID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if inbox, ok := m.inbox[agentID]; ok {
		close(inbox)
		delete(m.inbox, agentID)
	}
}
