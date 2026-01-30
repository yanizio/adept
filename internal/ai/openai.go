// internal/ai/openai.go
//
// OpenAI provider implementation.

package ai

import (
	"context"
	"fmt"

	"github.com/yanizio/adept/internal/api"
)

const openAIBaseURL = "https://api.openai.com/v1"

// OpenAI implements Chat and Embed via the OpenAI API.
type OpenAI struct {
	client     *api.Client
	apiKey     string
	chatModel  string
	embedModel string
}

// NewOpenAI returns an OpenAI provider.
func NewOpenAI(client *api.Client, apiKey, chatModel, embedModel string) *OpenAI {
	return &OpenAI{
		client:     client,
		apiKey:     apiKey,
		chatModel:  chatModel,
		embedModel: embedModel,
	}
}

// Chat requests a completion from OpenAI.
func (o *OpenAI) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	model := req.Model
	if model == "" {
		model = o.chatModel
	}

	payload := map[string]any{
		"model":    model,
		"messages": mapMessages(req.Messages),
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}

	var resp openAIChatResponse
	_, err := o.client.DoJSON(ctx, "POST", openAIBaseURL+"/chat/completions", payload, &resp, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", o.apiKey),
	})
	if err != nil {
		return ChatResponse{}, err
	}
	if len(resp.Choices) == 0 {
		return ChatResponse{}, fmt.Errorf("ai: openai returned no choices")
	}
	return ChatResponse{Content: resp.Choices[0].Message.Content, Model: resp.Model}, nil
}

// Embed requests embeddings from OpenAI.
func (o *OpenAI) Embed(ctx context.Context, req EmbedRequest) (EmbedResponse, error) {
	model := req.Model
	if model == "" {
		model = o.embedModel
	}
	payload := map[string]any{
		"model": model,
		"input": req.Inputs,
	}

	var resp openAIEmbedResponse
	_, err := o.client.DoJSON(ctx, "POST", openAIBaseURL+"/embeddings", payload, &resp, map[string]string{
		"Authorization": fmt.Sprintf("Bearer %s", o.apiKey),
	})
	if err != nil {
		return EmbedResponse{}, err
	}
	if len(resp.Data) == 0 {
		return EmbedResponse{}, fmt.Errorf("ai: openai returned no embeddings")
	}
	out := make([][]float32, 0, len(resp.Data))
	for _, item := range resp.Data {
		out = append(out, item.Embedding)
	}
	return EmbedResponse{Model: resp.Model, Embeddings: out}, nil
}

func mapMessages(msgs []Message) []map[string]string {
	out := make([]map[string]string, 0, len(msgs))
	for _, msg := range msgs {
		out = append(out, map[string]string{
			"role":    msg.Role,
			"content": msg.Content,
		})
	}
	return out
}

type openAIChatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

type openAIEmbedResponse struct {
	Model string `json:"model"`
	Data  []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}
