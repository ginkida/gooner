package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gokin/internal/logging"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SSHConfig holds connection configuration.
type SSHConfig struct {
	Host           string
	Port           int
	User           string
	KeyPath        string
	KeyPassphrase  string
	Password       string        // Fallback if no key
	Timeout        time.Duration
	KnownHostsPath string
}

// DefaultSSHConfig returns a configuration with sensible defaults.
func DefaultSSHConfig() *SSHConfig {
	currentUser, _ := user.Current()
	username := "root"
	homeDir := ""
	if currentUser != nil {
		username = currentUser.Username
		homeDir = currentUser.HomeDir
	}

	return &SSHConfig{
		Port:           22,
		User:           username,
		KeyPath:        filepath.Join(homeDir, ".ssh", "id_rsa"),
		Timeout:        30 * time.Second,
		KnownHostsPath: filepath.Join(homeDir, ".ssh", "known_hosts"),
	}
}

// SSHClient manages SSH connections.
type SSHClient struct {
	config  *SSHConfig
	conn    *ssh.Client
	mu      sync.Mutex
	lastUse time.Time
}

// NewSSHClient creates a new SSH client.
func NewSSHClient(config *SSHConfig) *SSHClient {
	return &SSHClient{
		config:  config,
		lastUse: time.Now(),
	}
}

// Connect establishes SSH connection.
func (c *SSHClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		// Check if connection is still alive
		_, _, err := c.conn.SendRequest("keepalive@openssh.com", true, nil)
		if err == nil {
			c.lastUse = time.Now()
			return nil // Connection still good
		}
		// Connection dead, close and reconnect
		c.conn.Close()
		c.conn = nil
	}

	// Build SSH client config
	sshConfig, err := c.buildSSHConfig()
	if err != nil {
		return fmt.Errorf("failed to build SSH config: %w", err)
	}

	// Dial with timeout
	addr := fmt.Sprintf("%s:%d", c.config.Host, c.config.Port)
	logging.Info("connecting to SSH", "addr", addr, "user", c.config.User)

	// Create connection with context deadline
	var conn net.Conn
	dialer := &net.Dialer{Timeout: c.config.Timeout}

	conn, err = dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", addr, err)
	}

	// Perform SSH handshake
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, sshConfig)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SSH handshake failed: %w", err)
	}

	c.conn = ssh.NewClient(sshConn, chans, reqs)
	c.lastUse = time.Now()

	logging.Info("SSH connection established", "host", c.config.Host)
	return nil
}

// buildSSHConfig creates the ssh.ClientConfig.
func (c *SSHClient) buildSSHConfig() (*ssh.ClientConfig, error) {
	var authMethods []ssh.AuthMethod

	// Try key-based authentication first
	if c.config.KeyPath != "" {
		keyPath := expandPath(c.config.KeyPath)
		if _, err := os.Stat(keyPath); err == nil {
			key, err := os.ReadFile(keyPath)
			if err != nil {
				logging.Warn("failed to read SSH key", "path", keyPath, "error", err)
			} else {
				var signer ssh.Signer
				if c.config.KeyPassphrase != "" {
					signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(c.config.KeyPassphrase))
				} else {
					signer, err = ssh.ParsePrivateKey(key)
				}
				if err != nil {
					logging.Warn("failed to parse SSH key", "path", keyPath, "error", err)
				} else {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
				}
			}
		}
	}

	// Try other common key files
	if len(authMethods) == 0 {
		for _, keyFile := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			keyPath := expandPath(filepath.Join("~/.ssh", keyFile))
			if key, err := os.ReadFile(keyPath); err == nil {
				if signer, err := ssh.ParsePrivateKey(key); err == nil {
					authMethods = append(authMethods, ssh.PublicKeys(signer))
					break
				}
			}
		}
	}

	// Fallback to password authentication
	if c.config.Password != "" {
		authMethods = append(authMethods, ssh.Password(c.config.Password))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no authentication method available")
	}

	// Host key callback - use InsecureIgnoreHostKey for now
	// In production, should verify against known_hosts
	hostKeyCallback := ssh.InsecureIgnoreHostKey()

	return &ssh.ClientConfig{
		User:            c.config.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         c.config.Timeout,
	}, nil
}

// Execute runs a command on the remote host.
func (c *SSHClient) Execute(ctx context.Context, command string) (string, int, error) {
	if err := c.Connect(ctx); err != nil {
		return "", -1, err
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	session, err := conn.NewSession()
	if err != nil {
		return "", -1, fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Set up output capture
	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr

	// Run command with context cancellation support
	done := make(chan error, 1)
	go func() {
		done <- session.Run(command)
	}()

	select {
	case <-ctx.Done():
		session.Signal(ssh.SIGKILL)
		return "", -1, ctx.Err()
	case err := <-done:
		c.lastUse = time.Now()
		output := stdout.String()
		if stderr.Len() > 0 {
			if output != "" {
				output += "\nSTDERR:\n"
			}
			output += stderr.String()
		}

		exitCode := 0
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				exitCode = exitErr.ExitStatus()
			} else {
				return output, -1, fmt.Errorf("command failed: %w", err)
			}
		}

		return output, exitCode, nil
	}
}

// Upload copies a local file to remote host via SFTP.
func (c *SSHClient) Upload(ctx context.Context, localPath, remotePath string) error {
	if err := c.Connect(ctx); err != nil {
		return err
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	// Create SFTP client
	sftpClient, err := sftp.NewClient(conn)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer sftpClient.Close()

	// Open local file
	localFile, err := os.Open(expandPath(localPath))
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer localFile.Close()

	// Get local file info for permissions
	localInfo, err := localFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat local file: %w", err)
	}

	// Create remote file
	remoteFile, err := sftpClient.Create(remotePath)
	if err != nil {
		return fmt.Errorf("failed to create remote file: %w", err)
	}
	defer remoteFile.Close()

	// Copy content
	_, err = io.Copy(remoteFile, localFile)
	if err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	// Set permissions
	if err := sftpClient.Chmod(remotePath, localInfo.Mode()); err != nil {
		logging.Warn("failed to set remote file permissions", "error", err)
	}

	c.lastUse = time.Now()
	logging.Info("file uploaded", "local", localPath, "remote", remotePath)
	return nil
}

// Download copies a remote file to local host via SFTP.
func (c *SSHClient) Download(ctx context.Context, remotePath, localPath string) error {
	if err := c.Connect(ctx); err != nil {
		return err
	}

	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()

	// Create SFTP client
	sftpClient, err := sftp.NewClient(conn)
	if err != nil {
		return fmt.Errorf("failed to create SFTP client: %w", err)
	}
	defer sftpClient.Close()

	// Open remote file
	remoteFile, err := sftpClient.Open(remotePath)
	if err != nil {
		return fmt.Errorf("failed to open remote file: %w", err)
	}
	defer remoteFile.Close()

	// Get remote file info
	remoteInfo, err := remoteFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat remote file: %w", err)
	}

	// Create local file
	localFile, err := os.Create(expandPath(localPath))
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}
	defer localFile.Close()

	// Copy content
	_, err = io.Copy(localFile, remoteFile)
	if err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	// Set permissions
	if err := os.Chmod(localPath, remoteInfo.Mode()); err != nil {
		logging.Warn("failed to set local file permissions", "error", err)
	}

	c.lastUse = time.Now()
	logging.Info("file downloaded", "remote", remotePath, "local", localPath)
	return nil
}

// Close closes the SSH connection.
func (c *SSHClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}

// IsConnected checks if connection is alive.
func (c *SSHClient) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return false
	}

	// Send keepalive request to check connection
	_, _, err := c.conn.SendRequest("keepalive@openssh.com", true, nil)
	return err == nil
}

// LastUse returns the time of last activity.
func (c *SSHClient) LastUse() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastUse
}

// SessionKey returns a unique key for this session.
func (c *SSHClient) SessionKey() string {
	return fmt.Sprintf("%s@%s:%d", c.config.User, c.config.Host, c.config.Port)
}

// expandPath expands ~ to home directory.
func expandPath(path string) string {
	if strings.HasPrefix(path, "~/") {
		if usr, err := user.Current(); err == nil {
			return filepath.Join(usr.HomeDir, path[2:])
		}
	}
	return path
}
