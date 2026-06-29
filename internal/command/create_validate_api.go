package command

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const anthropicMessagesURL = "https://api.anthropic.com/v1/messages"

// defaultCallAnthropicAPI makes a direct one-shot call to the Anthropic
// Messages API, bypassing the full claude CLI cold-start. Used by
// validateSBARSemantic (Layer 2+3) where no tool access is needed.
//
// Degrades gracefully: returns a non-nil error when ANTHROPIC_API_KEY is
// absent or the call fails. validateSBARSemantic treats any error as a skip
// (same as a CLI subprocess failure) so items are never blocked by a transient
// API hiccup.
func defaultCallAnthropicAPI(model, prompt string) ([]byte, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	resolvedModel := resolveAPIModelID(model)

	payload := map[string]interface{}{
		"model":      resolvedModel,
		"max_tokens": 512,
		"messages": []map[string]interface{}{
			{"role": "user", "content": prompt},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, anthropicMessagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API call: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse API response: %w", err)
	}
	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s", apiResp.Error.Message)
	}

	for _, c := range apiResp.Content {
		if c.Type == "text" {
			return []byte(c.Text), nil
		}
	}
	return nil, fmt.Errorf("no text content in API response")
}

// resolveAPIModelID maps short tier names ("haiku", "sonnet", "opus") to
// canonical model IDs for the Messages API. Full IDs pass through unchanged.
func resolveAPIModelID(model string) string {
	switch strings.ToLower(model) {
	case "haiku":
		return "claude-haiku-4-5"
	case "sonnet":
		return "claude-sonnet-4-6"
	case "opus":
		return "claude-opus-4-8"
	default:
		return model
	}
}
