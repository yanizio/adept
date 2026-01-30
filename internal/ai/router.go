// internal/ai/router.go
//
// Provider router that selects an AI backend based on config.

package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/yanizio/adept/internal/api"
)

// Chatter provides chat completions.
type Chatter interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// Embedder provides embeddings.
type Embedder interface {
	Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error)
}

// Router exposes the selected AI provider.
type Router struct {
	ChatProvider  Chatter
	EmbedProvider Embedder
	ProviderName  string
}

// NewRouter selects the provider based on config.
func NewRouter(config map[string]string, creds map[string]string) (*Router, error) {
	provider := strings.ToLower(strings.TrimSpace(config["ai.provider"]))
	if provider == "" {
		provider = "openai"
	}

	switch provider {
	case "openai":
		key := creds["openai"]
		if key == "" {
			return nil, fmt.Errorf("ai: openai api key missing")
		}
		client := api.New(api.Options{})
		chatModel := fallback(config["ai.chat.model"], "gpt-4o-mini")
		embedModel := fallback(config["ai.embed.model"], "text-embedding-3-small")
		openaiClient := NewOpenAI(client, key, chatModel, embedModel)
		return &Router{
			ChatProvider:  openaiClient,
			EmbedProvider: openaiClient,
			ProviderName:  "openai",
		}, nil
	default:
		return nil, fmt.Errorf("ai: unsupported provider %q", provider)
	}
}

func fallback(val, def string) string {
	if strings.TrimSpace(val) == "" {
		return def
	}
	return val
}
