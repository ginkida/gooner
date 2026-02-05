package auth

import (
	"os"
	"time"
)

// Google OAuth credentials for Gemini Code Assist
// Set via environment variables: GEMINI_OAUTH_CLIENT_ID, GEMINI_OAUTH_CLIENT_SECRET
var (
	GeminiOAuthClientID     = os.Getenv("GEMINI_OAUTH_CLIENT_ID")
	GeminiOAuthClientSecret = os.Getenv("GEMINI_OAUTH_CLIENT_SECRET")
)

const (
	GeminiOAuthRedirectURI  = "http://localhost:8085/oauth2callback"
	GeminiOAuthCallbackPort = 8085
)

// Google OAuth endpoints
const (
	GoogleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL = "https://oauth2.googleapis.com/token"
	GoogleUserInfo = "https://www.googleapis.com/oauth2/v2/userinfo"
)

// Gemini Code Assist API endpoint (uses OAuth, not API key)
const (
	GeminiCodeAssistAPI = "https://cloudcode-pa.googleapis.com"
)

// Code Assist API headers (required for proper request routing)
var CodeAssistHeaders = map[string]string{
	"User-Agent":      "google-api-nodejs-client/9.15.1",
	"X-Goog-Api-Client": "gl-node/22.17.0",
	"Client-Metadata": "ideType=IDE_UNSPECIFIED,platform=PLATFORM_UNSPECIFIED,pluginType=GEMINI",
}

// OAuth scopes
const (
	ScopeCloudPlatform   = "https://www.googleapis.com/auth/cloud-platform"
	ScopeUserInfoEmail   = "https://www.googleapis.com/auth/userinfo.email"
	ScopeUserInfoProfile = "https://www.googleapis.com/auth/userinfo.profile"
)

// OAuth timeouts
const (
	OAuthCallbackTimeout = 5 * time.Minute
	OAuthHTTPTimeout     = 30 * time.Second
	TokenRefreshBuffer   = 5 * time.Minute // Refresh token 5 minutes before expiry
)

// OAuthToken represents stored OAuth credentials
type OAuthToken struct {
	AccessToken  string    `json:"access_token" yaml:"access_token"`
	RefreshToken string    `json:"refresh_token" yaml:"refresh_token"`
	TokenType    string    `json:"token_type" yaml:"token_type"`
	ExpiresAt    time.Time `json:"expires_at" yaml:"expires_at"`
	Email        string    `json:"email,omitempty" yaml:"email,omitempty"`
}

// IsExpired checks if the token needs to be refreshed
func (t *OAuthToken) IsExpired() bool {
	if t == nil {
		return true
	}
	return time.Now().After(t.ExpiresAt.Add(-TokenRefreshBuffer))
}

// IsValid checks if the token is valid (has refresh token and access token)
func (t *OAuthToken) IsValid() bool {
	if t == nil {
		return false
	}
	return t.RefreshToken != "" && t.AccessToken != ""
}

// tokenResponse is the JSON response from Google's token endpoint
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`
}

// userInfoResponse is the JSON response from Google's userinfo endpoint
type userInfoResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	VerifiedEmail bool   `json:"verified_email"`
	Name          string `json:"name,omitempty"`
	Picture       string `json:"picture,omitempty"`
}
