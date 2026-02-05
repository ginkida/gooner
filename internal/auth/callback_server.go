package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CallbackServer handles the OAuth redirect callback
type CallbackServer struct {
	port          int
	expectedState string
	codeChan      chan string
	errChan       chan error
	server        *http.Server
}

// NewCallbackServer creates a new callback server
func NewCallbackServer(port int, expectedState string) *CallbackServer {
	return &CallbackServer{
		port:          port,
		expectedState: expectedState,
		codeChan:      make(chan string, 1),
		errChan:       make(chan error, 1),
	}
}

// Start starts the callback server in the background
func (s *CallbackServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2callback", s.handleCallback)

	s.server = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Start server in goroutine
	go func() {
		if err := s.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.errChan <- fmt.Errorf("callback server error: %w", err)
		}
	}()

	// Give server time to start
	time.Sleep(100 * time.Millisecond)
	return nil
}

// Stop stops the callback server
func (s *CallbackServer) Stop() {
	if s.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.server.Shutdown(ctx)
	}
}

// WaitForCode waits for the authorization code from the callback
func (s *CallbackServer) WaitForCode(timeout time.Duration) (string, error) {
	select {
	case code := <-s.codeChan:
		return code, nil
	case err := <-s.errChan:
		return "", err
	case <-time.After(timeout):
		return "", fmt.Errorf("timeout waiting for OAuth callback (did you complete the login in browser?)")
	}
}

// handleCallback handles the OAuth callback request
func (s *CallbackServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	// Check for error
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		errDesc := r.URL.Query().Get("error_description")
		s.errChan <- fmt.Errorf("OAuth error: %s - %s", errMsg, errDesc)
		s.renderResponse(w, false, "Authentication failed: "+errMsg)
		return
	}

	// Validate state
	state := r.URL.Query().Get("state")
	if state != s.expectedState {
		s.errChan <- fmt.Errorf("state mismatch: expected %s, got %s", s.expectedState, state)
		s.renderResponse(w, false, "Invalid state parameter (possible CSRF attack)")
		return
	}

	// Get authorization code
	code := r.URL.Query().Get("code")
	if code == "" {
		s.errChan <- fmt.Errorf("no authorization code received")
		s.renderResponse(w, false, "No authorization code received")
		return
	}

	// Send code to channel
	s.codeChan <- code
	s.renderResponse(w, true, "Authentication successful! You can close this window.")
}

// renderResponse renders an HTML response page
func (s *CallbackServer) renderResponse(w http.ResponseWriter, success bool, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	icon := "&#10060;" // Red X
	color := "#d32f2f"
	title := "Authentication Failed"
	if success {
		icon = "&#10004;" // Green checkmark
		color = "#388e3c"
		title = "Authentication Successful"
	}

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <title>Gokin - %s</title>
    <style>
        body {
            font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
            display: flex;
            justify-content: center;
            align-items: center;
            height: 100vh;
            margin: 0;
            background-color: #1a1a1a;
            color: #ffffff;
        }
        .container {
            text-align: center;
            padding: 40px;
            background-color: #2d2d2d;
            border-radius: 12px;
            box-shadow: 0 4px 6px rgba(0, 0, 0, 0.3);
        }
        .icon {
            font-size: 48px;
            margin-bottom: 20px;
            color: %s;
        }
        h1 {
            margin: 0 0 10px 0;
            font-size: 24px;
            font-weight: 600;
        }
        p {
            margin: 0;
            color: #b0b0b0;
            font-size: 14px;
        }
    </style>
</head>
<body>
    <div class="container">
        <div class="icon">%s</div>
        <h1>%s</h1>
        <p>%s</p>
    </div>
</body>
</html>`, title, color, icon, title, message)

	w.Write([]byte(html))
}
