package semantic

import (
	"context"
	"fmt"

	"google.golang.org/genai"
)

// Embedder generates embeddings using Gemini API.
type Embedder struct {
	client *genai.Client
	model  string
}

// NewEmbedder creates a new embedder.
func NewEmbedder(client *genai.Client, model string) *Embedder {
	if model == "" {
		model = "text-embedding-004"
	}
	return &Embedder{
		client: client,
		model:  model,
	}
}

// Embed generates an embedding for a single text.
func (e *Embedder) Embed(ctx context.Context, text string) ([]float32, error) {
	embeddings, err := e.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return embeddings[0], nil
}

// EmbedBatch generates embeddings for multiple texts.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Build content parts for embedding
	contents := make([]*genai.Content, len(texts))
	for i, text := range texts {
		contents[i] = &genai.Content{
			Parts: []*genai.Part{{Text: text}},
		}
	}

	// Call embedding API
	resp, err := e.client.Models.EmbedContent(ctx, e.model, contents, nil)
	if err != nil {
		return nil, fmt.Errorf("embedding API error: %w", err)
	}

	// Extract embeddings
	embeddings := make([][]float32, len(resp.Embeddings))
	for i, emb := range resp.Embeddings {
		embeddings[i] = emb.Values
	}

	return embeddings, nil
}

// GetModel returns the embedding model name.
func (e *Embedder) GetModel() string {
	return e.model
}
