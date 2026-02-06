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
// Splits into groups of maxBatchSize items to avoid API limits.
func (e *Embedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	const maxBatchSize = 20

	// If small enough, send in one call
	if len(texts) <= maxBatchSize {
		return e.embedBatchSingle(ctx, texts)
	}

	// Split into groups of maxBatchSize and concatenate results
	allEmbeddings := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxBatchSize {
		end := start + maxBatchSize
		if end > len(texts) {
			end = len(texts)
		}

		select {
		case <-ctx.Done():
			return allEmbeddings, ctx.Err()
		default:
		}

		batch := texts[start:end]
		embeddings, err := e.embedBatchSingle(ctx, batch)
		if err != nil {
			return allEmbeddings, fmt.Errorf("embedding batch %d-%d failed: %w", start, end, err)
		}
		allEmbeddings = append(allEmbeddings, embeddings...)
	}

	return allEmbeddings, nil
}

// embedBatchSingle sends a single batch of texts to the embedding API.
func (e *Embedder) embedBatchSingle(ctx context.Context, texts []string) ([][]float32, error) {
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
