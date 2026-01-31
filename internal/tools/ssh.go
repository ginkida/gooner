package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gooner/internal/logging"
	"gooner/internal/security"
	"gooner/internal/ssh"
	"gooner/internal/tasks"

	"google.golang.org/genai"
)

// SSHTool executes commands on remote servers via SSH.
type SSHTool struct {
	sessionManager *ssh.SessionManager
	taskManager    *tasks.Manager
	validator      *security.SSHValidator
	defaultTimeout time.Duration
}

// NewSSHTool creates a new SSH tool.
func NewSSHTool() *SSHTool {
	return &SSHTool{
		sessionManager: ssh.NewSessionManager(),
		validator:      security.NewSSHValidator(),
		defaultTimeout: 30 * time.Second,
	}
}

// SetTaskManager sets the task manager for background execution.
func (t *SSHTool) SetTaskManager(manager *tasks.Manager) {
	t.taskManager = manager
}

// SetValidator sets a custom SSH validator.
func (t *SSHTool) SetValidator(validator *security.SSHValidator) {
	t.validator = validator
}

// GetSessionManager returns the session manager for cleanup.
func (t *SSHTool) GetSessionManager() *ssh.SessionManager {
	return t.sessionManager
}

func (t *SSHTool) Name() string {
	return "ssh"
}

func (t *SSHTool) Description() string {
	return `Execute commands on remote servers via SSH.

Features:
- Persistent sessions (reuses connections)
- Key-based and password authentication
- File transfer (upload/download)
- Background execution for long commands

PARAMETERS:
- host (required): SSH server hostname or IP address
- port (optional): SSH port (default: 22)
- user (optional): SSH username (default: current user)
- command (optional): Command to execute on remote host
- action (optional): Action type - 'execute' (default), 'upload', 'download', 'list_sessions', 'close_session'
- local_path (optional): Local file path for upload/download
- remote_path (optional): Remote file path for upload/download
- key_path (optional): Path to SSH private key (default: ~/.ssh/id_rsa)
- run_in_background (optional): Run command in background, returns task_id
- timeout (optional): Command timeout in seconds (default: 30)

EXAMPLES:
- Execute command: ssh(host="server.com", command="ls -la")
- With specific user: ssh(host="server.com", user="admin", command="df -h")
- Upload file: ssh(host="server.com", action="upload", local_path="/tmp/file", remote_path="/home/user/file")
- Download file: ssh(host="server.com", action="download", remote_path="/var/log/app.log", local_path="/tmp/app.log")
- List sessions: ssh(action="list_sessions")
- Close session: ssh(host="server.com", action="close_session")
- Background: ssh(host="server.com", command="./deploy.sh", run_in_background=true)

SECURITY:
- Blocked: fork bombs, rm -rf /, disk writes
- Blocked hosts: localhost, 127.0.0.1
- Uses key-based authentication by default`
}

func (t *SSHTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"host": {
					Type:        genai.TypeString,
					Description: "SSH server hostname or IP address",
				},
				"port": {
					Type:        genai.TypeInteger,
					Description: "SSH port (default: 22)",
				},
				"user": {
					Type:        genai.TypeString,
					Description: "SSH username (default: current user)",
				},
				"command": {
					Type:        genai.TypeString,
					Description: "Command to execute on remote host",
				},
				"action": {
					Type:        genai.TypeString,
					Description: "Action: 'execute' (default), 'upload', 'download', 'list_sessions', 'close_session'",
					Enum:        []string{"execute", "upload", "download", "list_sessions", "close_session"},
				},
				"local_path": {
					Type:        genai.TypeString,
					Description: "Local file path for upload/download",
				},
				"remote_path": {
					Type:        genai.TypeString,
					Description: "Remote file path for upload/download",
				},
				"key_path": {
					Type:        genai.TypeString,
					Description: "Path to SSH private key (default: ~/.ssh/id_rsa)",
				},
				"run_in_background": {
					Type:        genai.TypeBoolean,
					Description: "Run command in background, returns task_id",
				},
				"timeout": {
					Type:        genai.TypeInteger,
					Description: "Command timeout in seconds (default: 30)",
				},
			},
			Required: []string{}, // host is required for most actions but not list_sessions
		},
	}
}

func (t *SSHTool) Validate(args map[string]any) error {
	action := GetStringDefault(args, "action", "execute")

	// list_sessions doesn't require host
	if action == "list_sessions" {
		return nil
	}

	// All other actions require host
	host, ok := GetString(args, "host")
	if !ok || host == "" {
		return NewValidationError("host", "host is required")
	}

	// Validate host
	if result := t.validator.ValidateHost(host); !result.Valid {
		return NewValidationError("host", result.Reason)
	}

	// Validate command if present
	if command, ok := GetString(args, "command"); ok && command != "" {
		if result := t.validator.ValidateCommand(command); !result.Valid {
			return NewValidationError("command", result.Reason)
		}
	}

	// Validate action-specific requirements
	switch action {
	case "execute":
		if _, ok := GetString(args, "command"); !ok {
			return NewValidationError("command", "command is required for execute action")
		}
	case "upload":
		if _, ok := GetString(args, "local_path"); !ok {
			return NewValidationError("local_path", "local_path is required for upload action")
		}
		if _, ok := GetString(args, "remote_path"); !ok {
			return NewValidationError("remote_path", "remote_path is required for upload action")
		}
	case "download":
		if _, ok := GetString(args, "remote_path"); !ok {
			return NewValidationError("remote_path", "remote_path is required for download action")
		}
		if _, ok := GetString(args, "local_path"); !ok {
			return NewValidationError("local_path", "local_path is required for download action")
		}
	}

	return nil
}

func (t *SSHTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	action := GetStringDefault(args, "action", "execute")

	switch action {
	case "execute":
		return t.executeCommand(ctx, args)
	case "upload":
		return t.uploadFile(ctx, args)
	case "download":
		return t.downloadFile(ctx, args)
	case "list_sessions":
		return t.listSessions()
	case "close_session":
		return t.closeSession(args)
	default:
		return NewErrorResult("unknown action: " + action), nil
	}
}

// buildConfig creates SSHConfig from args.
func (t *SSHTool) buildConfig(args map[string]any) *ssh.SSHConfig {
	config := ssh.DefaultSSHConfig()

	if host, ok := GetString(args, "host"); ok {
		config.Host = host
	}
	if port, ok := GetInt(args, "port"); ok && port > 0 {
		config.Port = port
	}
	if user, ok := GetString(args, "user"); ok && user != "" {
		config.User = user
	}
	if keyPath, ok := GetString(args, "key_path"); ok && keyPath != "" {
		config.KeyPath = keyPath
	}
	if timeout, ok := GetInt(args, "timeout"); ok && timeout > 0 {
		config.Timeout = time.Duration(timeout) * time.Second
	} else {
		config.Timeout = t.defaultTimeout
	}

	return config
}

func (t *SSHTool) executeCommand(ctx context.Context, args map[string]any) (ToolResult, error) {
	command, _ := GetString(args, "command")
	runInBackground := GetBoolDefault(args, "run_in_background", false)

	if runInBackground {
		return t.executeBackground(ctx, args, command)
	}

	config := t.buildConfig(args)

	// Get or create session
	client, err := t.sessionManager.GetOrCreate(ctx, config)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to connect: %s", err)), nil
	}

	// Execute command
	logging.Info("executing SSH command", "host", config.Host, "command", command)
	output, exitCode, err := client.Execute(ctx, command)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("command failed: %s", err)), nil
	}

	// Truncate long output
	const maxLen = 30000
	if len(output) > maxLen {
		totalLen := len(output)
		output = output[:maxLen] + fmt.Sprintf("\n... (output truncated: showing %d of %d characters)", maxLen, totalLen)
	}

	if output == "" {
		output = "(no output)"
	}

	if exitCode != 0 {
		return ToolResult{
			Content: output,
			Error:   fmt.Sprintf("command exited with code %d", exitCode),
			Success: false,
		}, nil
	}

	return NewSuccessResultWithData(output, map[string]any{
		"host":      config.Host,
		"exit_code": exitCode,
	}), nil
}

func (t *SSHTool) executeBackground(ctx context.Context, args map[string]any, command string) (ToolResult, error) {
	if t.taskManager == nil {
		return NewErrorResult("background tasks not configured"), nil
	}

	config := t.buildConfig(args)

	// Create wrapped command that includes SSH
	sshCommand := fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o BatchMode=yes %s@%s -p %d '%s'",
		config.User, config.Host, config.Port, command)

	taskID, err := t.taskManager.Start(ctx, sshCommand)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to start background task: %s", err)), nil
	}

	return NewSuccessResultWithData(
		fmt.Sprintf("Started background SSH task: %s\nHost: %s\nCommand: %s\nUse task_output tool with task_id=\"%s\" to check status.",
			taskID, config.Host, command, taskID),
		map[string]any{
			"task_id":    taskID,
			"background": true,
			"host":       config.Host,
		},
	), nil
}

func (t *SSHTool) uploadFile(ctx context.Context, args map[string]any) (ToolResult, error) {
	localPath, _ := GetString(args, "local_path")
	remotePath, _ := GetString(args, "remote_path")
	config := t.buildConfig(args)

	// Get or create session
	client, err := t.sessionManager.GetOrCreate(ctx, config)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to connect: %s", err)), nil
	}

	// Upload file
	logging.Info("uploading file via SSH", "local", localPath, "remote", remotePath, "host", config.Host)
	if err := client.Upload(ctx, localPath, remotePath); err != nil {
		return NewErrorResult(fmt.Sprintf("upload failed: %s", err)), nil
	}

	return NewSuccessResult(fmt.Sprintf("File uploaded successfully: %s -> %s@%s:%s",
		localPath, config.User, config.Host, remotePath)), nil
}

func (t *SSHTool) downloadFile(ctx context.Context, args map[string]any) (ToolResult, error) {
	remotePath, _ := GetString(args, "remote_path")
	localPath, _ := GetString(args, "local_path")
	config := t.buildConfig(args)

	// Get or create session
	client, err := t.sessionManager.GetOrCreate(ctx, config)
	if err != nil {
		return NewErrorResult(fmt.Sprintf("failed to connect: %s", err)), nil
	}

	// Download file
	logging.Info("downloading file via SSH", "remote", remotePath, "local", localPath, "host", config.Host)
	if err := client.Download(ctx, remotePath, localPath); err != nil {
		return NewErrorResult(fmt.Sprintf("download failed: %s", err)), nil
	}

	return NewSuccessResult(fmt.Sprintf("File downloaded successfully: %s@%s:%s -> %s",
		config.User, config.Host, remotePath, localPath)), nil
}

func (t *SSHTool) listSessions() (ToolResult, error) {
	sessions := t.sessionManager.List()

	if len(sessions) == 0 {
		return NewSuccessResult("No active SSH sessions."), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Active SSH sessions (%d):\n\n", len(sessions)))

	for _, s := range sessions {
		status := "connected"
		if !s.Connected {
			status = "disconnected"
		}
		sb.WriteString(fmt.Sprintf("  - %s [%s] (idle: %s)\n", s.Key, status, s.IdleTime.Round(time.Second)))
	}

	return NewSuccessResult(sb.String()), nil
}

func (t *SSHTool) closeSession(args map[string]any) (ToolResult, error) {
	host, _ := GetString(args, "host")
	port := GetIntDefault(args, "port", 22)
	user := GetStringDefault(args, "user", "")

	// Build session key
	config := t.buildConfig(args)
	key := fmt.Sprintf("%s@%s:%d", config.User, host, port)

	// Try to find and close the session
	if user != "" {
		key = fmt.Sprintf("%s@%s:%d", user, host, port)
	}

	if err := t.sessionManager.Close(key); err != nil {
		// Try with just host:port pattern
		sessions := t.sessionManager.List()
		for _, s := range sessions {
			if s.Host == host && s.Port == port {
				if err := t.sessionManager.Close(s.Key); err == nil {
					return NewSuccessResult(fmt.Sprintf("Session closed: %s", s.Key)), nil
				}
			}
		}
		return NewErrorResult(fmt.Sprintf("failed to close session: %s", err)), nil
	}

	return NewSuccessResult(fmt.Sprintf("Session closed: %s", key)), nil
}

// Cleanup closes all SSH sessions. Should be called on application shutdown.
func (t *SSHTool) Cleanup() {
	if t.sessionManager != nil {
		t.sessionManager.Stop()
	}
}
