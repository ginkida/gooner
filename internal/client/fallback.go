package client

import (
	"context"
	"fmt"
	"sync"

	"gokin/internal/logging"

	"google.golang.org/genai"
)

// FallbackClient wraps multiple Client instances and tries each in order
// on failure, providing automatic failover between providers.
type FallbackClient struct {
	clients []Client
	current int
	mu      sync.RWMutex
}

// NewFallbackClient creates a new FallbackClient with the given clients.
// At least one client must be provided.
func NewFallbackClient(clients []Client) (*FallbackClient, error) {
	if len(clients) == 0 {
		return nil, fmt.Errorf("fallback client requires at least one client")
	}
	return &FallbackClient{
		clients: clients,
		current: 0,
	}, nil
}

// getCurrent returns the current active client index.
func (fc *FallbackClient) getCurrent() int {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.current
}

// advance moves to the next client in the chain. Returns false if no more clients.
func (fc *FallbackClient) advance() bool {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.current+1 < len(fc.clients) {
		fc.current++
		logging.Warn("falling back to next client",
			"index", fc.current,
			"model", fc.clients[fc.current].GetModel())
		return true
	}
	return false
}

// resetCurrent resets back to the first client.
func (fc *FallbackClient) resetCurrent() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.current = 0
}

// SendMessage sends a message, trying fallback clients on error.
func (fc *FallbackClient) SendMessage(ctx context.Context, message string) (*StreamingResponse, error) {
	startIdx := fc.getCurrent()
	for i := startIdx; i < len(fc.clients); i++ {
		fc.mu.Lock()
		fc.current = i
		fc.mu.Unlock()

		resp, err := fc.clients[i].SendMessage(ctx, message)
		if err == nil {
			return resp, nil
		}

		logging.Warn("client failed in SendMessage",
			"index", i,
			"model", fc.clients[i].GetModel(),
			"error", err.Error())

		// If context is cancelled, don't try next client
		if ctx.Err() != nil {
			return nil, err
		}

		// If this is the last client, return the error
		if i+1 >= len(fc.clients) {
			return nil, fmt.Errorf("all fallback clients failed, last error: %w", err)
		}
	}
	return nil, fmt.Errorf("all fallback clients exhausted")
}

// SendMessageWithHistory sends a message with history, trying fallback clients on error.
func (fc *FallbackClient) SendMessageWithHistory(ctx context.Context, history []*genai.Content, message string) (*StreamingResponse, error) {
	startIdx := fc.getCurrent()
	for i := startIdx; i < len(fc.clients); i++ {
		fc.mu.Lock()
		fc.current = i
		fc.mu.Unlock()

		resp, err := fc.clients[i].SendMessageWithHistory(ctx, history, message)
		if err == nil {
			return resp, nil
		}

		logging.Warn("client failed in SendMessageWithHistory",
			"index", i,
			"model", fc.clients[i].GetModel(),
			"error", err.Error())

		if ctx.Err() != nil {
			return nil, err
		}

		if i+1 >= len(fc.clients) {
			return nil, fmt.Errorf("all fallback clients failed, last error: %w", err)
		}
	}
	return nil, fmt.Errorf("all fallback clients exhausted")
}

// SendFunctionResponse sends function results, trying fallback clients on error.
func (fc *FallbackClient) SendFunctionResponse(ctx context.Context, history []*genai.Content, results []*genai.FunctionResponse) (*StreamingResponse, error) {
	startIdx := fc.getCurrent()
	for i := startIdx; i < len(fc.clients); i++ {
		fc.mu.Lock()
		fc.current = i
		fc.mu.Unlock()

		resp, err := fc.clients[i].SendFunctionResponse(ctx, history, results)
		if err == nil {
			return resp, nil
		}

		logging.Warn("client failed in SendFunctionResponse",
			"index", i,
			"model", fc.clients[i].GetModel(),
			"error", err.Error())

		if ctx.Err() != nil {
			return nil, err
		}

		if i+1 >= len(fc.clients) {
			return nil, fmt.Errorf("all fallback clients failed, last error: %w", err)
		}
	}
	return nil, fmt.Errorf("all fallback clients exhausted")
}

// SetSystemInstruction sets the system-level instruction on ALL clients in the fallback chain.
func (fc *FallbackClient) SetSystemInstruction(instruction string) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	for _, c := range fc.clients {
		c.SetSystemInstruction(instruction)
	}
}

// SetThinkingBudget sets thinking budget on ALL clients in the fallback chain.
func (fc *FallbackClient) SetThinkingBudget(budget int32) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	for _, c := range fc.clients {
		c.SetThinkingBudget(budget)
	}
}

// SetTools sets tools on ALL clients in the fallback chain.
func (fc *FallbackClient) SetTools(tools []*genai.Tool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	for _, c := range fc.clients {
		c.SetTools(tools)
	}
}

// SetRateLimiter sets the rate limiter on ALL clients in the fallback chain.
func (fc *FallbackClient) SetRateLimiter(limiter interface{}) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	for _, c := range fc.clients {
		c.SetRateLimiter(limiter)
	}
}

// CountTokens counts tokens using the current active client.
func (fc *FallbackClient) CountTokens(ctx context.Context, contents []*genai.Content) (*genai.CountTokensResponse, error) {
	idx := fc.getCurrent()
	return fc.clients[idx].CountTokens(ctx, contents)
}

// GetModel returns the current active client's model name.
func (fc *FallbackClient) GetModel() string {
	idx := fc.getCurrent()
	return fc.clients[idx].GetModel()
}

// SetModel changes the model on the current active client.
func (fc *FallbackClient) SetModel(modelName string) {
	idx := fc.getCurrent()
	fc.clients[idx].SetModel(modelName)
}

// WithModel returns a new client configured for the specified model.
// Uses the current active client's WithModel implementation.
func (fc *FallbackClient) WithModel(modelName string) Client {
	idx := fc.getCurrent()
	return fc.clients[idx].WithModel(modelName)
}

// GetRawClient returns the current active client's raw client.
func (fc *FallbackClient) GetRawClient() interface{} {
	idx := fc.getCurrent()
	return fc.clients[idx].GetRawClient()
}

// Close closes ALL clients in the fallback chain.
func (fc *FallbackClient) Close() error {
	fc.mu.Lock()
	defer fc.mu.Unlock()

	var lastErr error
	for _, c := range fc.clients {
		if err := c.Close(); err != nil {
			lastErr = err
		}
	}
	return lastErr
}
