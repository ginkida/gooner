package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OAuthManager handles the OAuth 2.0 flow for Gemini authentication
type OAuthManager struct {
	clientID     string
	clientSecret string
	redirectURI  string
	scopes       []string

	// PKCE state (set during GenerateAuthURL)
	codeVerifier  string
	codeChallenge string
	state         string

	httpClient *http.Client
}

// NewGeminiOAuthManager creates an OAuthManager with Gemini Code Assist credentials
func NewGeminiOAuthManager() *OAuthManager {
	return &OAuthManager{
		clientID:     GeminiOAuthClientID,
		clientSecret: GeminiOAuthClientSecret,
		redirectURI:  GeminiOAuthRedirectURI,
		scopes: []string{
			ScopeCloudPlatform,
			ScopeUserInfoEmail,
			ScopeUserInfoProfile,
		},
		httpClient: &http.Client{
			Timeout: OAuthHTTPTimeout,
		},
	}
}

// GenerateAuthURL creates the authorization URL with PKCE parameters.
// The user should be directed to this URL to authenticate.
func (m *OAuthManager) GenerateAuthURL() (string, error) {
	// Generate PKCE parameters
	var err error
	m.codeVerifier, err = generateCodeVerifier()
	if err != nil {
		return "", fmt.Errorf("failed to generate code verifier: %w", err)
	}
	m.codeChallenge = generateCodeChallenge(m.codeVerifier)
	m.state, err = generateState()
	if err != nil {
		return "", fmt.Errorf("failed to generate state: %w", err)
	}

	params := url.Values{
		"client_id":             {m.clientID},
		"redirect_uri":          {m.redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(m.scopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {m.codeChallenge},
		"code_challenge_method": {"S256"},
		"state":                 {m.state},
	}

	// Add # at the end to prevent trailing parameters from being added
	return GoogleAuthURL + "?" + params.Encode() + "#", nil
}

// GetState returns the current state parameter for validation
func (m *OAuthManager) GetState() string {
	return m.state
}

// ExchangeCode exchanges the authorization code for access and refresh tokens
func (m *OAuthManager) ExchangeCode(ctx context.Context, code string) (*OAuthToken, error) {
	data := url.Values{
		"client_id":     {m.clientID},
		"client_secret": {m.clientSecret},
		"code":          {code},
		"code_verifier": {m.codeVerifier},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {m.redirectURI},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", GoogleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to exchange code: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse token response: %w", err)
	}

	token := &OAuthToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}

	// Fetch user email
	email, err := m.fetchUserEmail(ctx, token.AccessToken)
	if err != nil {
		// Non-fatal - continue without email
		email = ""
	}
	token.Email = email

	return token, nil
}

// RefreshToken refreshes an expired access token using the refresh token
func (m *OAuthManager) RefreshToken(ctx context.Context, refreshToken string) (*OAuthToken, error) {
	data := url.Values{
		"client_id":     {m.clientID},
		"client_secret": {m.clientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}

	req, err := http.NewRequestWithContext(ctx, "POST", GoogleTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to refresh token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read refresh response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token refresh failed (%d): %s", resp.StatusCode, string(body))
	}

	var tokenResp tokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to parse refresh response: %w", err)
	}

	token := &OAuthToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: refreshToken, // Refresh token doesn't change
		TokenType:    tokenResp.TokenType,
		ExpiresAt:    time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second),
	}

	// If a new refresh token was provided, use it
	if tokenResp.RefreshToken != "" {
		token.RefreshToken = tokenResp.RefreshToken
	}

	return token, nil
}

// fetchUserEmail retrieves the user's email from Google userinfo endpoint
func (m *OAuthManager) fetchUserEmail(ctx context.Context, accessToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", GoogleUserInfo, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo request failed: %d", resp.StatusCode)
	}

	var userInfo userInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&userInfo); err != nil {
		return "", err
	}

	return userInfo.Email, nil
}
