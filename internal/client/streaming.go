package client

import (
	"context"

	"google.golang.org/genai"
)

// StreamHandler provides callbacks for handling streaming responses.
type StreamHandler struct {
	// OnText is called for each text chunk received.
	OnText func(text string)

	// OnFunctionCall is called for each function call received.
	OnFunctionCall func(fc *genai.FunctionCall)

	// OnError is called when an error occurs.
	OnError func(err error)

	// OnComplete is called when the response is complete.
	OnComplete func(response *Response)
}

// ProcessStream processes a streaming response with the given handler.
func ProcessStream(ctx context.Context, sr *StreamingResponse, handler *StreamHandler) (*Response, error) {
	resp := &Response{}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case chunk, ok := <-sr.Chunks:
			if !ok {
				if handler.OnComplete != nil {
					handler.OnComplete(resp)
				}
				return resp, nil
			}

			if chunk.Error != nil {
				if handler.OnError != nil {
					handler.OnError(chunk.Error)
				}
				return nil, chunk.Error
			}

			if chunk.Text != "" {
				resp.Text += chunk.Text
				if handler.OnText != nil {
					handler.OnText(chunk.Text)
				}
			}

			for _, fc := range chunk.FunctionCalls {
				resp.FunctionCalls = append(resp.FunctionCalls, fc)
				// CRITICAL: Also add to Parts so history reconstruction works correctly
				// Without this, tool_use blocks are missing when we rebuild assistant messages
				resp.Parts = append(resp.Parts, &genai.Part{FunctionCall: fc})
				if handler.OnFunctionCall != nil {
					handler.OnFunctionCall(fc)
				}
			}

			// Accumulate original Parts (preserves ThoughtSignature for Gemini 3).
			// Skip FunctionCall parts since they are already added explicitly above;
			// including them again causes duplicate FunctionCalls in history which
			// triggers Gemini API 400 errors (mismatched call/response count).
			for _, part := range chunk.Parts {
				if part != nil && part.FunctionCall == nil {
					resp.Parts = append(resp.Parts, part)
				}
			}

			// Keep the latest non-zero usage metadata (typically from the final chunk)
			if chunk.InputTokens > 0 {
				resp.InputTokens = chunk.InputTokens
			}
			if chunk.OutputTokens > 0 {
				resp.OutputTokens += chunk.OutputTokens
			}

			if chunk.Done {
				resp.FinishReason = chunk.FinishReason
				if handler.OnComplete != nil {
					handler.OnComplete(resp)
				}
				return resp, nil
			}
		}
	}
}

// CollectText is a convenience function that collects only text from a stream.
func CollectText(ctx context.Context, sr *StreamingResponse) (string, error) {
	resp, err := ProcessStream(ctx, sr, &StreamHandler{})
	if err != nil {
		return "", err
	}
	return resp.Text, nil
}
