package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Config holds provider connection info resolved at execution time.
type Config struct {
	// Ollama (OpenAI-compatible) settings
	OllamaURL    string
	OllamaAPIKey string
	OllamaModel  string
	// OllamaSmallModel backs the free in-product helpers. It is deliberately a
	// small, cheap model: these calls are a courtesy paid for by the platform
	// key, not by the user's own credits.
	OllamaSmallModel string
	// Google AI Studio settings
	GoogleAPIKey string
	// GeminiBaseURL is where the Gemini calls go. It exists so a test can point
	// them at a loopback server: the image and vision nodes are the two that
	// cannot be exercised at all without one, and a test that reached the real
	// endpoint would be a test that spends money and fails on a train.
	GeminiBaseURL string
	// ImageModel and VisionModel are the platform defaults a node falls back to
	// when it does not name a model of its own.
	ImageModel  string
	VisionModel string

	// Black Forest Labs, the second image backend. The platform holds this one
	// so that somebody who has just signed up can generate an image before
	// being asked for a credential of their own.
	BFLAPIKey string
	// BFLBaseURL exists for the same reason GeminiBaseURL does: a test that
	// reached the real endpoint would spend money and fail on a train.
	BFLBaseURL string
	// ImageProvider is the deployment default when a node names no provider.
	ImageProvider string

	// PinnedTextModel forces every text call onto one model, whatever a node
	// asked for. It exists for runs the platform pays for: a free allowance
	// where a node could name the most expensive model on the endpoint is not
	// an allowance, it is an invitation. Empty means the node chooses, which is
	// what every run on somebody's own credential does.
	PinnedTextModel string

	// Anthropic and OpenAI, reached with the user's own credential. Anthropic
	// accepts either a console API key or an OAuth token; the adapter tells
	// them apart and sends the matching header.
	AnthropicAPIKey string
	AnthropicModel  string
	// AnthropicBaseURL allows pointing at a gateway or a test server.
	AnthropicBaseURL string
	OpenAIAPIKey     string
	OpenAIBaseURL    string
	OpenAIModel      string

	// Local CLI providers run the user's own installed agent CLI as a
	// subprocess. They are how a ChatGPT or Claude *subscription* powers a
	// workflow: the CLI is already authenticated on this machine, so no
	// credential ever reaches Zyvro. Only usable when the engine runs on the
	// user's machine (the desktop app), never on the hosted server.
	ClaudeCLIPath string
	CodexCLIPath  string
	// ClaudeCLIModel and CodexCLIModel are empty by default, which lets the CLI
	// keep whatever model the subscription already chose for it.
	ClaudeCLIModel string
	CodexCLIModel  string
	// LocalCLITimeout caps one CLI call. Zero means 10 minutes.
	LocalCLITimeout time.Duration
	// LocalCLIWorkdir is the directory the CLI runs in. Empty means a temp dir.
	LocalCLIWorkdir string

	// TextProvider is the platform default for text nodes and the Brain when a
	// node does not name one: "ollama", "anthropic", "openai", "claude-cli" or
	// "codex-cli".
	TextProvider string
}

// FromEnv builds provider config from environment (server-side .creds).
func FromEnv() *Config {
	return &Config{
		OllamaURL:    getEnv("OLLAMA_URL", "https://ollama.com"),
		OllamaAPIKey: os.Getenv("OLLAMA_API_KEY"),
		OllamaModel:  getEnv("OLLAMA_MODEL", "qwen3.5:397b"),
		// gemma4:31b answers this kind of one-liner in ~25 tokens. The other
		// small options on the cloud endpoint emit reasoning tokens that eat
		// the whole budget before producing any content.
		OllamaSmallModel: getEnv("OLLAMA_SMALL_MODEL", "gemma4:31b"),
		GoogleAPIKey:     os.Getenv("AI_STUDIO_GOOGLE_API_KEY"),
		GeminiBaseURL:    getEnv("GEMINI_BASE_URL", defaultGeminiBaseURL),
		ImageModel:       getEnv("GEMINI_IMAGE_MODEL", "gemini-3.1-flash-image"),
		// Named for the model it buys rather than for the vendor, because that
		// is the name on the account this key belongs to.
		BFLAPIKey:     os.Getenv("KLEIN_CLOUD_API_KEY"),
		BFLBaseURL:    getEnv("KLEIN_CLOUD_BASE_URL", bflDefaultBaseURL),
		ImageProvider: getEnv("IMAGE_PROVIDER", ""),
		VisionModel:   getEnv("GEMINI_VISION_MODEL", "gemini-3.6-flash"),

		AnthropicAPIKey:  os.Getenv("ANTHROPIC_API_KEY"),
		AnthropicModel:   getEnv("ANTHROPIC_MODEL", "claude-opus-5"),
		AnthropicBaseURL: getEnv("ANTHROPIC_BASE_URL", "https://api.anthropic.com"),
		OpenAIAPIKey:     os.Getenv("OPENAI_API_KEY"),
		OpenAIBaseURL:    getEnv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
		OpenAIModel:      getEnv("OPENAI_MODEL", "gpt-5.1"),

		ClaudeCLIPath: getEnv("CLAUDE_CLI_PATH", "claude"),
		CodexCLIPath:  getEnv("CODEX_CLI_PATH", "codex"),
		// Deliberately unset: an empty model means "let the CLI choose", which is
		// the only correct answer for a subscription the user configured itself.
		ClaudeCLIModel:  os.Getenv("CLAUDE_CLI_MODEL"),
		CodexCLIModel:   os.Getenv("CODEX_CLI_MODEL"),
		LocalCLIWorkdir: os.Getenv("LOCAL_CLI_WORKDIR"),

		TextProvider: getEnv("TEXT_PROVIDER", "ollama"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type ProviderError struct {
	Code    string
	Message string
	Status  int
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func normalizeHTTPError(status int, body []byte) error {
	code := "unknown"
	switch {
	case status == 401 || status == 403:
		code = "auth_error"
	case status == 429:
		code = "rate_limit"
	case status == 400:
		code = "invalid_input"
	case status >= 500:
		code = "provider_down"
	}
	return &ProviderError{Code: code, Message: truncate(string(body), 300), Status: status}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var httpClient = &http.Client{Timeout: 300 * time.Second}

// defaultGeminiBaseURL is where the Gemini calls go when nothing says otherwise,
// which is every deployment. Config.GeminiBaseURL overrides it.
const defaultGeminiBaseURL = "https://generativelanguage.googleapis.com"

// geminiURL builds the generateContent endpoint for a model. It is a method so
// that a Config with no base URL — one built as a literal rather than by
// FromEnv, which is how the tests and some embedders do it — still reaches the
// real endpoint rather than a URL beginning with a slash.
func (c *Config) geminiURL(model string) string {
	base := defaultGeminiBaseURL
	if c != nil && c.GeminiBaseURL != "" {
		base = strings.TrimSuffix(c.GeminiBaseURL, "/")
	}
	return fmt.Sprintf("%s/v1beta/models/%s:generateContent", base, model)
}

// ---------- LLM (Ollama, OpenAI-compatible) ----------

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string     `json:"type"`
	Function ToolSchema `json:"function"`
}

type ToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type LLMRequest struct {
	Messages []Message
	Tools    []Tool
	// Provider overrides the deployment's text provider for this call:
	// "ollama", "anthropic" or "openai". Empty uses the default.
	Provider string
	// Model overrides the configured chat model for this call. Empty uses the
	// provider's configured model.
	Model       string
	MaxTokens   int
	Temperature float64
}

type LLMResponse struct {
	Content   string
	ToolCalls []ToolCall
}

type ollamaChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	MaxTokens   int       `json:"max_tokens,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

type ollamaChatResponse struct {
	Choices []struct {
		Message struct {
			Role      string     `json:"role"`
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
		} `json:"message"`
		// FinishReason is why the model stopped. "length" means it hit the cap,
		// which is the one case where an empty answer has an explanation worth
		// passing on.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ollamaComplete calls the Ollama OpenAI-compatible chat completions endpoint.
// Reached through LLMComplete, which picks the provider.
func (c *Config) ollamaComplete(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	model := req.Model
	if model == "" {
		model = c.OllamaModel
	}
	payload := ollamaChatRequest{
		Model:       model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
	}
	body, _ := json.Marshal(payload)
	url := strings.TrimSuffix(c.OllamaURL, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.OllamaAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.OllamaAPIKey)
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, normalizeHTTPError(resp.StatusCode, raw)
	}
	var parsed ollamaChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid LLM response: %s", truncate(string(raw), 200))
	}
	if parsed.Error != nil {
		return nil, &ProviderError{Code: "provider_error", Message: parsed.Error.Message}
	}
	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("empty LLM response")
	}
	choice := parsed.Choices[0]
	if err := emptyCompletion(choice.Message.Content, len(choice.Message.ToolCalls), choice.FinishReason, parsed.Usage.CompletionTokens, req.MaxTokens); err != nil {
		return nil, err
	}
	return &LLMResponse{
		Content:   choice.Message.Content,
		ToolCalls: choice.Message.ToolCalls,
	}, nil
}

// ---------- Vision (Gemini VLM) ----------

type VisionRequest struct {
	Instruction string
	Images      [][]byte // raw image bytes
	MimeTypes   []string
}

func (c *Config) GeminiVision(ctx context.Context, model, instruction string, images [][]byte, mimeTypes []string) (string, error) {
	parts := []map[string]any{}
	for i, img := range images {
		mime := "image/png"
		if i < len(mimeTypes) && mimeTypes[i] != "" {
			mime = mimeTypes[i]
		}
		parts = append(parts, map[string]any{
			"inlineData": map[string]string{
				"mimeType": mime,
				"data":     base64.StdEncoding.EncodeToString(img),
			},
		})
	}
	parts = append(parts, map[string]any{"text": instruction})
	payload := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": parts},
		},
	}
	body, _ := json.Marshal(payload)
	url := c.geminiURL(model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.GoogleAPIKey)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return "", normalizeHTTPError(resp.StatusCode, raw)
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("invalid vision response: %s", truncate(string(raw), 200))
	}
	if parsed.Error != nil {
		return "", &ProviderError{Code: "provider_error", Message: parsed.Error.Message}
	}
	var sb strings.Builder
	for _, cand := range parsed.Candidates {
		for _, part := range cand.Content.Parts {
			sb.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// ---------- Image generation/editing (Gemini) ----------

type ImageResult struct {
	Data     []byte
	MimeType string
}

// GeminiImageGenerate generates an image from a prompt and optional references.
func (c *Config) GeminiImageGenerate(ctx context.Context, model, prompt, aspectRatio, imageSize string, references []ImageResult) (*ImageResult, error) {
	parts := []map[string]any{}
	for _, ref := range references {
		parts = append(parts, map[string]any{
			"inlineData": map[string]string{
				"mimeType": ref.MimeType,
				"data":     base64.StdEncoding.EncodeToString(ref.Data),
			},
		})
	}
	parts = append(parts, map[string]any{"text": prompt})
	payload := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": parts},
		},
	}
	if aspectRatio != "" || imageSize != "" {
		imageConfig := map[string]any{}
		if aspectRatio != "" {
			imageConfig["aspectRatio"] = aspectRatio
		}
		if imageSize != "" {
			imageConfig["imageSize"] = imageSize
		}
		payload["generationConfig"] = map[string]any{"imageConfig": imageConfig}
	}
	body, _ := json.Marshal(payload)
	url := c.geminiURL(model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.GoogleAPIKey)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != 200 {
		return nil, normalizeHTTPError(resp.StatusCode, raw)
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("invalid image response: %s", truncate(string(raw), 200))
	}
	if parsed.Error != nil {
		return nil, &ProviderError{Code: "provider_error", Message: parsed.Error.Message}
	}
	for _, cand := range parsed.Candidates {
		for _, part := range cand.Content.Parts {
			if part.InlineData != nil && part.InlineData.Data != "" {
				data, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
				if err != nil {
					return nil, err
				}
				mime := part.InlineData.MimeType
				if mime == "" {
					mime = "image/png"
				}
				return &ImageResult{Data: data, MimeType: mime}, nil
			}
		}
	}
	return nil, fmt.Errorf("no image returned by Gemini")
}

// ---------- Gemini structured text (fallback for LLM via Gemini if needed) ----------

func (c *Config) GeminiText(ctx context.Context, model, system, user string) (string, error) {
	payload := map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]any{{"text": user}}},
		},
	}
	if system != "" {
		payload["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}
	body, _ := json.Marshal(payload)
	url := c.geminiURL(model)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.GoogleAPIKey)
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return "", normalizeHTTPError(resp.StatusCode, raw)
	}
	var parsed struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("invalid text response: %s", truncate(string(raw), 200))
	}
	var sb strings.Builder
	for _, cand := range parsed.Candidates {
		for _, part := range cand.Content.Parts {
			sb.WriteString(part.Text)
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// emptyCompletion turns a model that answered with nothing into an error that
// says why.
//
// A reasoning model spends tokens thinking before it writes, and the cap covers
// both. Ask it for a sentence within two hundred tokens and it can use every
// one of them reasoning and emit no content at all — a response the provider
// reports as a success, with finish_reason "length" and an empty string.
//
// Returning that empty string as a result is the worst of the options. The node
// reports success, the emptiness travels downstream, and the run fails three
// nodes later on something like "generate image needs a prompt" — which sends
// whoever is reading it to look at the wrong node. Failing here says which node
// failed, and the one thing that fixes it.
//
// Tool calls are an answer: a model that called a tool and wrote no prose did
// exactly what it was asked to.
func emptyCompletion(content string, toolCalls int, finishReason string, completionTokens, maxTokens int) error {
	if strings.TrimSpace(content) != "" || toolCalls > 0 {
		return nil
	}
	if finishReason == "length" {
		budget := "its token budget"
		if maxTokens > 0 {
			budget = fmt.Sprintf("its budget of %d tokens", maxTokens)
		}
		spent := ""
		if completionTokens > 0 {
			spent = fmt.Sprintf(" (it spent %d)", completionTokens)
		}
		return &ProviderError{
			Code: "empty_completion",
			Message: fmt.Sprintf(
				"the model used %s before writing anything%s. Raise Max tokens on this node, or pick a model that does not reason before answering",
				budget, spent),
		}
	}
	return &ProviderError{
		Code:    "empty_completion",
		Message: "the model returned no text",
	}
}
