package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gokin/internal/security"

	"google.golang.org/genai"
)

// SearchProvider defines the search backend to use.
type SearchProvider string

const (
	SearchProviderSerpAPI SearchProvider = "serpapi"
	SearchProviderGoogle  SearchProvider = "google"
)

// WebSearchTool performs web searches using external APIs.
type WebSearchTool struct {
	client     *http.Client
	provider   SearchProvider
	apiKey     string
	googleCX   string // Google Custom Search Engine ID
	maxResults int
}

// NewWebSearchTool creates a new web search tool.
func NewWebSearchTool() *WebSearchTool {
	// Create secure HTTP client with TLS 1.2+ enforcement
	secureClient, err := security.CreateDefaultHTTPClient()
	if err != nil {
		// Fall back to default client if secure client creation fails
		secureClient = &http.Client{
			Timeout: 30 * time.Second,
		}
	}

	return &WebSearchTool{
		client:     secureClient,
		provider:   SearchProviderSerpAPI,
		maxResults: 10,
	}
}

// SetAPIKey sets the API key for the search provider.
func (t *WebSearchTool) SetAPIKey(key string) {
	t.apiKey = key
}

// SetProvider sets the search provider.
func (t *WebSearchTool) SetProvider(provider SearchProvider) {
	t.provider = provider
}

// SetGoogleCX sets the Google Custom Search Engine ID.
func (t *WebSearchTool) SetGoogleCX(cx string) {
	t.googleCX = cx
}

func (t *WebSearchTool) Name() string {
	return "web_search"
}

func (t *WebSearchTool) Description() string {
	return "Searches the web and returns relevant results. Useful for finding current information, documentation, or research."
}

func (t *WebSearchTool) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters: &genai.Schema{
			Type: genai.TypeObject,
			Properties: map[string]*genai.Schema{
				"query": {
					Type:        genai.TypeString,
					Description: "The search query",
				},
				"num_results": {
					Type:        genai.TypeInteger,
					Description: "Number of results to return (default 5, max 10)",
				},
			},
			Required: []string{"query"},
		},
	}
}

func (t *WebSearchTool) Validate(args map[string]any) error {
	query, ok := GetString(args, "query")
	if !ok || query == "" {
		return NewValidationError("query", "is required")
	}

	if t.apiKey == "" {
		return NewValidationError("api_key", "web search API key not configured")
	}

	return nil
}

func (t *WebSearchTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	query, _ := GetString(args, "query")
	numResults := GetIntDefault(args, "num_results", 5)

	if numResults > t.maxResults {
		numResults = t.maxResults
	}
	if numResults < 1 {
		numResults = 5
	}

	var results []SearchResult
	var err error

	switch t.provider {
	case SearchProviderGoogle:
		results, err = t.searchGoogle(ctx, query, numResults)
	default:
		results, err = t.searchSerpAPI(ctx, query, numResults)
	}

	if err != nil {
		return NewErrorResult(fmt.Sprintf("search failed: %s", err)), nil
	}

	if len(results) == 0 {
		return NewSuccessResult("No results found for the query."), nil
	}

	// Format results
	var output strings.Builder
	output.WriteString(fmt.Sprintf("Search results for: %s\n\n", query))

	for i, r := range results {
		output.WriteString(fmt.Sprintf("%d. **%s**\n", i+1, r.Title))
		output.WriteString(fmt.Sprintf("   %s\n", r.URL))
		if r.Snippet != "" {
			output.WriteString(fmt.Sprintf("   %s\n", r.Snippet))
		}
		output.WriteString("\n")
	}

	// Convert to JSON for structured data
	resultsJSON, _ := json.Marshal(results)

	return NewSuccessResultWithData(output.String(), map[string]any{
		"query":   query,
		"count":   len(results),
		"results": string(resultsJSON),
	}), nil
}

// SearchResult represents a single search result.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchSerpAPI performs a search using SerpAPI.
// Note: SerpAPI requires the API key in query parameters per their API spec.
// We minimize exposure by not logging the full URL and using HTTPS.
func (t *WebSearchTool) searchSerpAPI(ctx context.Context, query string, numResults int) ([]SearchResult, error) {
	baseURL := "https://serpapi.com/search"

	params := url.Values{}
	params.Set("q", query)
	params.Set("engine", "google")
	params.Set("num", fmt.Sprintf("%d", numResults))

	// Build URL without API key first (for any potential logging)
	reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	// Add API key separately to minimize exposure in potential error messages
	params.Set("api_key", t.apiKey)
	fullURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		// Return error without exposing the full URL with API key
		return nil, fmt.Errorf("failed to create request for %s", reqURL)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// Return error without exposing the full URL with API key
		return nil, fmt.Errorf("request to %s failed: %w", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		OrganicResults []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic_results"`
		Error string `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if data.Error != "" {
		return nil, fmt.Errorf("API error: %s", data.Error)
	}

	results := make([]SearchResult, 0, len(data.OrganicResults))
	for _, r := range data.OrganicResults {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.Link,
			Snippet: r.Snippet,
		})
	}

	return results, nil
}

// searchGoogle performs a search using Google Custom Search API.
// Note: Google Custom Search API requires the API key in query parameters per their spec.
// We minimize exposure by not logging the full URL and using HTTPS.
func (t *WebSearchTool) searchGoogle(ctx context.Context, query string, numResults int) ([]SearchResult, error) {
	if t.googleCX == "" {
		return nil, fmt.Errorf("Google Custom Search Engine ID (cx) not configured")
	}

	baseURL := "https://www.googleapis.com/customsearch/v1"

	params := url.Values{}
	params.Set("q", query)
	params.Set("cx", t.googleCX)
	params.Set("num", fmt.Sprintf("%d", numResults))

	// Build URL without API key first (for any potential logging)
	reqURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	// Add API key separately to minimize exposure in potential error messages
	params.Set("key", t.apiKey)
	fullURL := fmt.Sprintf("%s?%s", baseURL, params.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", fullURL, nil)
	if err != nil {
		// Return error without exposing the full URL with API key
		return nil, fmt.Errorf("failed to create request for %s", reqURL)
	}

	resp, err := t.client.Do(req)
	if err != nil {
		// Return error without exposing the full URL with API key
		return nil, fmt.Errorf("request to %s failed: %w", reqURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var data struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if data.Error.Message != "" {
		return nil, fmt.Errorf("API error: %s", data.Error.Message)
	}

	results := make([]SearchResult, 0, len(data.Items))
	for _, r := range data.Items {
		results = append(results, SearchResult{
			Title:   r.Title,
			URL:     r.Link,
			Snippet: r.Snippet,
		})
	}

	return results, nil
}
