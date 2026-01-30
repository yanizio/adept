// internal/ai/types.go
//
// Provider-agnostic AI request and response types.

package ai

// Message represents a chat turn.
type Message struct {
	Role    string
	Content string
}

// ChatRequest is a provider-agnostic chat input.
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature float32
}

// ChatResponse returns assistant output.
type ChatResponse struct {
	Content string
	Model   string
}

// EmbedRequest is a provider-agnostic embedding input.
type EmbedRequest struct {
	Model  string
	Inputs []string
}

// EmbedResponse returns embedding vectors.
type EmbedResponse struct {
	Model      string
	Embeddings [][]float32
}
