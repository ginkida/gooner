package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"gokin/internal/logging"
)

// Transport defines the interface for MCP transport implementations.
type Transport interface {
	// Send sends a JSON-RPC message to the server.
	Send(msg *JSONRPCMessage) error

	// Receive receives a JSON-RPC message from the server.
	// Returns io.EOF when the transport is closed.
	Receive() (*JSONRPCMessage, error)

	// Close closes the transport connection.
	Close() error
}

// SafeEnvVars is the whitelist of environment variables passed to MCP server processes.
// This prevents leaking sensitive environment variables like API keys.
var SafeEnvVars = []string{
	"PATH",
	"HOME",
	"USER",
	"SHELL",
	"TERM",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"TMPDIR",
	"TMP",
	"TEMP",
	"XDG_CONFIG_HOME",
	"XDG_DATA_HOME",
	"XDG_CACHE_HOME",
	"XDG_RUNTIME_DIR",
	// Node/npm
	"NODE_PATH",
	"NPM_CONFIG_PREFIX",
	// Python
	"PYTHONPATH",
	"VIRTUAL_ENV",
}

// buildSafeEnv creates a sanitized environment for MCP server process execution.
func buildSafeEnv() []string {
	env := make([]string, 0, len(SafeEnvVars))
	for _, key := range SafeEnvVars {
		if val := os.Getenv(key); val != "" {
			env = append(env, key+"="+val)
		}
	}
	// Always set a safe PATH if not already set
	hasPath := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
			break
		}
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	return env
}

// StdioTransport communicates with an MCP server via stdio.
type StdioTransport struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser

	encoder *json.Encoder
	scanner *bufio.Scanner

	mu     sync.Mutex
	closed bool

	// For stderr logging
	stderrDone chan struct{}
}

// NewStdioTransport creates a new stdio transport by starting the specified command.
func NewStdioTransport(command string, args []string, env map[string]string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...)

	// Use sanitized environment to prevent leaking sensitive env vars
	cmd.Env = buildSafeEnv()

	// Add user-specified env vars with secret expansion
	for k, v := range env {
		expanded := os.ExpandEnv(v) // Expands ${VAR}
		cmd.Env = append(cmd.Env, k+"="+expanded)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			logging.Debug("error closing stdin during cleanup", "error", closeErr)
		}
		return nil, fmt.Errorf("failed to get stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			logging.Debug("error closing stdin during cleanup", "error", closeErr)
		}
		if closeErr := stdout.Close(); closeErr != nil {
			logging.Debug("error closing stdout during cleanup", "error", closeErr)
		}
		return nil, fmt.Errorf("failed to get stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if closeErr := stdin.Close(); closeErr != nil {
			logging.Debug("error closing stdin during cleanup", "error", closeErr)
		}
		if closeErr := stdout.Close(); closeErr != nil {
			logging.Debug("error closing stdout during cleanup", "error", closeErr)
		}
		if closeErr := stderr.Close(); closeErr != nil {
			logging.Debug("error closing stderr during cleanup", "error", closeErr)
		}
		return nil, fmt.Errorf("failed to start MCP server: %w", err)
	}

	t := &StdioTransport{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		encoder:    json.NewEncoder(stdin),
		scanner:    bufio.NewScanner(stdout),
		stderrDone: make(chan struct{}),
	}

	// Set a reasonable buffer size for the scanner (1MB)
	const maxScannerBuffer = 1024 * 1024
	t.scanner.Buffer(make([]byte, 0, 64*1024), maxScannerBuffer)

	// Start stderr logging goroutine
	go t.logStderr()

	logging.Debug("MCP stdio transport started",
		"command", command,
		"args", args,
		"pid", cmd.Process.Pid)

	return t, nil
}

// logStderr reads and logs stderr output from the MCP server.
func (t *StdioTransport) logStderr() {
	defer close(t.stderrDone)
	scanner := bufio.NewScanner(t.stderr)
	for scanner.Scan() {
		line := scanner.Text()
		logging.Debug("MCP server stderr", "line", line)
	}
}

// Send sends a JSON-RPC message to the server.
func (t *StdioTransport) Send(msg *JSONRPCMessage) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return fmt.Errorf("transport is closed")
	}

	// Ensure JSONRPC version is set
	msg.JSONRPC = "2.0"

	// Encode as JSON followed by newline (JSON-RPC over stdio convention)
	if err := t.encoder.Encode(msg); err != nil {
		return fmt.Errorf("failed to encode message: %w", err)
	}

	logging.Debug("MCP message sent",
		"method", msg.Method,
		"id", msg.ID)

	return nil
}

// Receive receives a JSON-RPC message from the server.
func (t *StdioTransport) Receive() (*JSONRPCMessage, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, io.EOF
	}
	t.mu.Unlock()

	// Read next line
	if !t.scanner.Scan() {
		if err := t.scanner.Err(); err != nil {
			return nil, fmt.Errorf("scanner error: %w", err)
		}
		return nil, io.EOF
	}

	line := t.scanner.Text()
	if line == "" {
		// Skip empty lines
		return t.Receive()
	}

	var msg JSONRPCMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return nil, fmt.Errorf("failed to parse JSON-RPC message: %w", err)
	}

	logging.Debug("MCP message received",
		"method", msg.Method,
		"id", msg.ID,
		"has_error", msg.Error != nil)

	return &msg, nil
}

// Close closes the transport and terminates the server process.
func (t *StdioTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true

	// Close stdin first to signal the server to exit
	if t.stdin != nil {
		t.stdin.Close()
	}

	// Wait for stderr logging to complete
	select {
	case <-t.stderrDone:
	case <-time.After(time.Second):
		// Don't wait forever
	}

	// Give the process a chance to exit gracefully
	done := make(chan error, 1)
	go func() {
		done <- t.cmd.Wait()
	}()

	select {
	case <-done:
		// Process exited cleanly
		logging.Debug("MCP server process exited")
	case <-time.After(5 * time.Second):
		// Kill if it doesn't exit in time
		logging.Warn("MCP server not responding, killing process")
		if t.cmd.Process != nil {
			t.cmd.Process.Kill()
		}
		<-done
	}

	return nil
}

// HTTPTransport communicates with an MCP server via HTTP (Streamable HTTP).
type HTTPTransport struct {
	url     string
	headers map[string]string
	timeout time.Duration

	// For receiving messages (long-polling or SSE)
	recvChan chan *JSONRPCMessage
	errChan  chan error

	mu     sync.Mutex
	closed bool
	ctx    context.Context
	cancel context.CancelFunc
}

// NewHTTPTransport creates a new HTTP transport.
func NewHTTPTransport(url string, headers map[string]string, timeout time.Duration) (*HTTPTransport, error) {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	ctx, cancel := context.WithCancel(context.Background())

	t := &HTTPTransport{
		url:      url,
		headers:  headers,
		timeout:  timeout,
		recvChan: make(chan *JSONRPCMessage, 10),
		errChan:  make(chan error, 1),
		ctx:      ctx,
		cancel:   cancel,
	}

	logging.Debug("MCP HTTP transport created", "url", url)

	return t, nil
}

// Send sends a JSON-RPC message to the server via HTTP POST.
func (t *HTTPTransport) Send(msg *JSONRPCMessage) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("transport is closed")
	}
	t.mu.Unlock()

	// Ensure JSONRPC version is set
	msg.JSONRPC = "2.0"

	// Serialize message
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %w", err)
	}

	// Create request with timeout
	ctx, cancel := context.WithTimeout(t.ctx, t.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}

	// Send request
	client := &http.Client{Timeout: t.timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check status
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, string(body))
	}

	// Parse response (if any)
	if resp.ContentLength > 0 || resp.ContentLength == -1 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read response body: %w", err)
		}

		if len(body) > 0 {
			var response JSONRPCMessage
			if err := json.Unmarshal(body, &response); err != nil {
				return fmt.Errorf("failed to decode response: %w", err)
			}

			// Send response to receive channel
			select {
			case t.recvChan <- &response:
			case <-t.ctx.Done():
				return t.ctx.Err()
			}
		}
	}

	logging.Debug("MCP HTTP message sent",
		"method", msg.Method,
		"id", msg.ID)

	return nil
}

// Receive receives a JSON-RPC message from the server.
func (t *HTTPTransport) Receive() (*JSONRPCMessage, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, io.EOF
	}
	t.mu.Unlock()

	select {
	case msg := <-t.recvChan:
		return msg, nil
	case err := <-t.errChan:
		return nil, err
	case <-t.ctx.Done():
		return nil, io.EOF
	}
}

// Close closes the HTTP transport.
func (t *HTTPTransport) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.closed {
		return nil
	}
	t.closed = true
	t.cancel()

	logging.Debug("MCP HTTP transport closed")
	return nil
}
