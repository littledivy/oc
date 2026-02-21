package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	anthropicAPIHost = "api.anthropic.com"
	anthropicModel   = "claude-opus-4-6"
	maxTokens        = 32000
	apiVersion       = "2023-06-01"
)

// Global callback token accumulators — updated by classifierChat calls.
var (
	callbackInputTokens  int
	callbackOutputTokens int
	callbackAPICalls     int
)

type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []ContentBlock
}

type ContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
}

type StreamEvent struct {
	Type                string // "text", "tool_use_start", "input_json", "block_stop", "message_stop", "message_delta"
	Text                string
	ToolID              string
	ToolName            string
	JSON                string
	StopReason          string
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

func streamChat(messages []Message, auth *AuthMethod, systemPrompt string) (*StreamReader, error) {
	if auth != nil && auth.IsOpenAI() {
		return streamChatOpenAI(messages, auth, systemPrompt)
	}

	reqBody := buildRequestBody(messages, systemPrompt)

	url := fmt.Sprintf("https://%s/v1/messages", anthropicAPIHost)
	if auth.IsOAuth() {
		url += "?beta=true"
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)

	if auth.IsOAuth() {
		req.Header.Set("Authorization", "Bearer "+auth.Token.AccessToken)
		req.Header.Set("anthropic-beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	} else {
		req.Header.Set("x-api-key", auth.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	return &StreamReader{
		scanner: bufio.NewScanner(resp.Body),
		body:    resp.Body,
	}, nil
}

type StreamReader struct {
	scanner *bufio.Scanner
	body    io.ReadCloser
	events  []StreamEvent
	nextIdx int
	mode    string
	oa      openAIResponseState
}

type openAIResponseState struct {
	itemToCallID map[string]string
	itemHasDelta map[string]bool
	hadToolCall  bool
}

func (sr *StreamReader) Close() {
	if sr.body != nil {
		sr.body.Close()
	}
}

func (sr *StreamReader) Next() (*StreamEvent, error) {
	if sr.nextIdx < len(sr.events) {
		ev := sr.events[sr.nextIdx]
		sr.nextIdx++
		return &ev, nil
	}

	if sr.scanner == nil {
		return nil, nil
	}

	for sr.scanner.Scan() {
		line := sr.scanner.Text()

		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := line[6:]
		if data == "[DONE]" {
			return &StreamEvent{Type: "message_stop"}, nil
		}

		if sr.mode == "openai_responses_sse" {
			evs := parseOpenAIResponsesSSEData(data, &sr.oa)
			if len(evs) == 0 {
				continue
			}
			sr.events = evs
			sr.nextIdx = 0
			ev := sr.events[sr.nextIdx]
			sr.nextIdx++
			return &ev, nil
		}

		return parseSSEData(data), nil
	}

	if err := sr.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, nil // EOF
}

func parseSSEData(data string) *StreamEvent {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return &StreamEvent{Type: "other"}
	}

	eventType, _ := raw["type"].(string)

	switch eventType {
	case "content_block_delta":
		delta, _ := raw["delta"].(map[string]any)
		if delta == nil {
			return &StreamEvent{Type: "other"}
		}
		deltaType, _ := delta["type"].(string)
		switch deltaType {
		case "text_delta":
			text, _ := delta["text"].(string)
			return &StreamEvent{Type: "text", Text: text}
		case "input_json_delta":
			j, _ := delta["partial_json"].(string)
			return &StreamEvent{Type: "input_json", JSON: j}
		case "thinking_delta":
			return &StreamEvent{Type: "other"}
		}
	case "content_block_start":
		cb, _ := raw["content_block"].(map[string]any)
		if cb == nil {
			return &StreamEvent{Type: "other"}
		}
		cbType, _ := cb["type"].(string)
		if cbType == "tool_use" {
			id, _ := cb["id"].(string)
			name, _ := cb["name"].(string)
			return &StreamEvent{Type: "tool_use_start", ToolID: id, ToolName: name}
		}
	case "content_block_stop":
		return &StreamEvent{Type: "block_stop"}
	case "message_stop":
		return &StreamEvent{Type: "message_stop"}
	case "message_start":
		msg, _ := raw["message"].(map[string]any)
		if msg != nil {
			usage, _ := msg["usage"].(map[string]any)
			if usage != nil {
				input, _ := usage["input_tokens"].(float64)
				cacheRead, _ := usage["cache_read_input_tokens"].(float64)
				cacheCreate, _ := usage["cache_creation_input_tokens"].(float64)
				return &StreamEvent{Type: "usage", InputTokens: int(input), CacheReadTokens: int(cacheRead), CacheCreationTokens: int(cacheCreate)}
			}
		}
	case "message_delta":
		delta, _ := raw["delta"].(map[string]any)
		if delta != nil {
			sr, _ := delta["stop_reason"].(string)
			ev := &StreamEvent{Type: "message_delta", StopReason: sr}
			usage, _ := raw["usage"].(map[string]any)
			if usage != nil {
				output, _ := usage["output_tokens"].(float64)
				ev.OutputTokens = int(output)
			}
			return ev
		}
	}

	return &StreamEvent{Type: "other"}
}

const classifierModel = "claude-haiku-4-5-20251001"

// classifierChat makes a non-streaming API call using Haiku for cheap classification.
// Used as a fallback when Ollama is unavailable.
func classifierChat(messages []Message, auth *AuthMethod) (string, error) {
	stopSpinner := startSpinner(nil)
	defer stopSpinner()

	if auth != nil && auth.IsOpenAI() {
		return classifierChatOpenAI(messages, auth)
	}

	reqBody := map[string]any{
		"model":       classifierModel,
		"max_tokens":  256,
		"temperature": 0.0,
		"stream":      false,
		"messages":    messages,
	}
	data, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("https://%s/v1/messages", anthropicAPIHost)
	if auth.IsOAuth() {
		url += "?beta=true"
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)

	if auth.IsOAuth() {
		req.Header.Set("Authorization", "Bearer "+auth.Token.AccessToken)
		req.Header.Set("anthropic-beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	} else {
		req.Header.Set("x-api-key", auth.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %v", err)
	}

	callbackInputTokens += result.Usage.InputTokens
	callbackOutputTokens += result.Usage.OutputTokens
	callbackAPICalls++

	var texts []string
	for _, block := range result.Content {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func compactionChat(messages []Message, auth *AuthMethod, sysPrompt string) (string, error) {
	if auth != nil && auth.IsOpenAI() {
		return compactionChatOpenAI(messages, auth, sysPrompt)
	}

	reqBody := map[string]any{
		"model":       anthropicModel,
		"max_tokens":  4096,
		"temperature": 0.0,
		"stream":      false,
		"messages":    messages,
	}
	if sysPrompt != "" {
		reqBody["system"] = sysPrompt
	}
	data, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("https://%s/v1/messages", anthropicAPIHost)
	if auth.IsOAuth() {
		url += "?beta=true"
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", apiVersion)
	if auth.IsOAuth() {
		req.Header.Set("Authorization", "Bearer "+auth.Token.AccessToken)
		req.Header.Set("anthropic-beta", "oauth-2025-04-20,interleaved-thinking-2025-05-14")
	} else {
		req.Header.Set("x-api-key", auth.APIKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("failed to parse response: %v", err)
	}
	var texts []string
	for _, block := range result.Content {
		if block.Type == "text" {
			texts = append(texts, block.Text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n")), nil
}

func buildRequestBody(messages []Message, systemPrompt string) []byte {
	req := map[string]any{
		"model":      anthropicModel,
		"max_tokens": maxTokens,
		"stream":     true,
		"tools":      json.RawMessage(buildToolDefinitions()),
		"messages":   applyCacheControl(messages),
	}
	if systemPrompt != "" {
		req["system"] = []map[string]any{
			{
				"type":          "text",
				"text":          systemPrompt,
				"cache_control": map[string]string{"type": "ephemeral"},
			},
		}
	}
	data, _ := json.Marshal(req)
	return data
}

// applyCacheControl marks the last 2 messages with cache_control so Anthropic
// caches the conversation prefix. The system prompt is cached separately.
func applyCacheControl(messages []Message) []map[string]any {
	out := make([]map[string]any, len(messages))
	mark := map[int]bool{}
	count := 0
	for i := len(messages) - 1; i >= 0 && count < 2; i-- {
		mark[i] = true
		count++
	}

	for i, msg := range messages {
		if !mark[i] {
			out[i] = map[string]any{"role": msg.Role, "content": msg.Content}
			continue
		}
		switch c := msg.Content.(type) {
		case string:
			out[i] = map[string]any{
				"role": msg.Role,
				"content": []map[string]any{
					{
						"type":          "text",
						"text":          c,
						"cache_control": map[string]string{"type": "ephemeral"},
					},
				},
			}
		case []ContentBlock:
			if len(c) == 0 {
				out[i] = map[string]any{"role": msg.Role, "content": c}
				continue
			}
			blocks := make([]map[string]any, len(c))
			for j, b := range c {
				block := map[string]any{"type": b.Type}
				if b.Text != "" {
					block["text"] = b.Text
				}
				if b.ID != "" {
					block["id"] = b.ID
				}
				if b.Name != "" {
					block["name"] = b.Name
				}
				if len(b.Input) > 0 {
					block["input"] = b.Input
				}
				if b.ToolUseID != "" {
					block["tool_use_id"] = b.ToolUseID
				}
				if b.Content != "" {
					block["content"] = b.Content
				}
				if j == len(c)-1 {
					block["cache_control"] = map[string]string{"type": "ephemeral"}
				}
				blocks[j] = block
			}
			out[i] = map[string]any{"role": msg.Role, "content": blocks}
		default:
			out[i] = map[string]any{"role": msg.Role, "content": msg.Content}
		}
	}
	return out
}

const (
	clientID            = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	redirectURI         = "https://console.anthropic.com/oauth/code/callback"
	scopes              = "org:create_api_key user:profile user:inference"
	tokenURL            = "https://console.anthropic.com/v1/oauth/token"
	openAIAuthIssuer    = "https://auth.openai.com"
	openAICodexClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAIOAuthPort     = 1455
)

type AuthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
}

type AuthMethod struct {
	Provider  string
	Token     *AuthToken
	APIKey    string
	AccountID string
}

type OpenAIConfig struct {
	APIKey    string     `json:"api_key,omitempty"`
	OAuth     *AuthToken `json:"oauth,omitempty"`
	AccountID string     `json:"account_id,omitempty"`
}

func (a *AuthMethod) IsOAuth() bool {
	return a.Token != nil
}

func (a *AuthMethod) IsOpenAI() bool {
	return a != nil && a.Provider == "openai"
}

func (a *AuthMethod) IsAnthropic() bool {
	return a != nil && a.Provider == "anthropic"
}

func getAuth() (*AuthMethod, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OC_PROVIDER"))) {
	case "openai":
		if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
			return &AuthMethod{Provider: "openai", APIKey: key}, nil
		}
		if auth, err := getSavedOpenAIAuth(); err == nil && auth != nil {
			return auth, nil
		}
		return nil, nil
	case "anthropic":
		return getAnthropicAuth()
	}

	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		return &AuthMethod{Provider: "openai", APIKey: key}, nil
	}
	if auth, err := getSavedOpenAIAuth(); err == nil && auth != nil {
		return auth, nil
	}

	return getAnthropicAuth()
}

func getSavedOpenAIAuth() (*AuthMethod, error) {
	cfg, err := loadOpenAIConfig()
	if err != nil {
		return nil, err
	}

	if cfg.OAuth != nil && strings.TrimSpace(cfg.OAuth.AccessToken) != "" {
		token := cfg.OAuth
		if token.ExpiresAt > 0 && time.Now().Unix() >= token.ExpiresAt-60 {
			if token.RefreshToken != "" {
				newToken, err := refreshOpenAIToken(token.RefreshToken)
				if err != nil {
					return nil, err
				}
				cfg.OAuth = newToken
				if err := saveOpenAIConfig(cfg); err != nil {
					return nil, err
				}
				token = newToken
			} else {
				return nil, nil
			}
		}
		return &AuthMethod{
			Provider:  "openai",
			Token:     token,
			AccountID: strings.TrimSpace(cfg.AccountID),
		}, nil
	}

	if key := strings.TrimSpace(cfg.APIKey); key != "" {
		return &AuthMethod{Provider: "openai", APIKey: key}, nil
	}

	return nil, nil
}

func ensureOpenAIToken(auth *AuthMethod) (string, string, error) {
	if auth == nil || !auth.IsOpenAI() {
		return "", "", fmt.Errorf("not an openai auth method")
	}
	if auth.Token == nil {
		if strings.TrimSpace(auth.APIKey) == "" {
			return "", "", fmt.Errorf("missing openai credentials")
		}
		return auth.APIKey, auth.AccountID, nil
	}

	if auth.Token.ExpiresAt > 0 && time.Now().Unix() >= auth.Token.ExpiresAt-60 {
		if auth.Token.RefreshToken == "" {
			return "", "", fmt.Errorf("openai oauth token expired and no refresh token is available")
		}
		newToken, err := refreshOpenAIToken(auth.Token.RefreshToken)
		if err != nil {
			return "", "", err
		}
		auth.Token = newToken

		cfg, err := loadOpenAIConfig()
		if err == nil {
			cfg.OAuth = newToken
			_ = saveOpenAIConfig(cfg)
		}
	}

	return auth.Token.AccessToken, auth.AccountID, nil
}

func getAnthropicAuth() (*AuthMethod, error) {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return &AuthMethod{Provider: "anthropic", APIKey: key}, nil
	}

	token, err := loadToken()
	if err != nil {
		return nil, nil
	}

	if token.ExpiresAt > 0 && time.Now().Unix() >= token.ExpiresAt-60 {
		if token.RefreshToken != "" {
			newToken, err := refreshToken(token.RefreshToken)
			if err != nil {
				return nil, nil
			}
			_ = saveToken(newToken)
			return &AuthMethod{Provider: "anthropic", Token: newToken}, nil
		}
		return nil, nil
	}

	return &AuthMethod{Provider: "anthropic", Token: token}, nil
}

func login() (*AuthToken, error) {
	verifier, challenge := generatePKCE()

	url := fmt.Sprintf(
		"https://claude.ai/oauth/authorize?code=true&client_id=%s&response_type=code&redirect_uri=%s&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s",
		clientID,
		urlEncode(redirectURI),
		urlEncode(scopes),
		challenge,
		verifier,
	)

	fmt.Printf("\n\033[1mOpen this URL in your browser to authorize:\033[0m\n\n")
	fmt.Printf("  \033[36m%s\033[0m\n\n", url)
	fmt.Printf("\033[1mPaste the authorization code here:\033[0m ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return nil, fmt.Errorf("no input")
	}
	raw := strings.TrimSpace(scanner.Text())
	if raw == "" {
		return nil, fmt.Errorf("no code provided")
	}

	code := raw
	state := ""
	if idx := strings.Index(raw, "#"); idx >= 0 {
		code = raw[:idx]
		state = raw[idx+1:]
	}

	fmt.Print("Exchanging code for token...")

	body := fmt.Sprintf(
		`{"code":"%s","state":"%s","grant_type":"authorization_code","client_id":"%s","redirect_uri":"%s","code_verifier":"%s"}`,
		code, state, clientID, redirectURI, verifier,
	)

	resp, err := http.Post(tokenURL, "application/json", strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("token request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("invalid response: %s", string(respBody))
	}

	if result.Error != "" {
		return nil, fmt.Errorf("auth error: %s %s", result.Error, result.ErrorDesc)
	}
	if result.AccessToken == "" {
		return nil, fmt.Errorf("no access token in response")
	}

	var expiresAt int64
	if result.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + result.ExpiresIn
	}

	token := &AuthToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    expiresAt,
	}

	fmt.Print("\r\033[K\033[32mAuthenticated!\033[0m\n")

	if err := saveToken(token); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not save token: %v\n", err)
	}

	return token, nil
}

func loginOpenAI() error {
	fmt.Printf("\n\033[1mOpenAI Login\033[0m\n")
	fmt.Printf("  1) ChatGPT Plus/Pro (browser OAuth)\n")
	fmt.Printf("  2) ChatGPT Plus/Pro (headless device code)\n")
	fmt.Printf("  3) API key\n")
	fmt.Printf("\nChoose method [1]: ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return fmt.Errorf("no input")
	}
	choice := strings.TrimSpace(scanner.Text())
	if choice == "" {
		choice = "1"
	}

	switch choice {
	case "1":
		token, accountID, err := loginOpenAIOAuthBrowser()
		if err != nil {
			return err
		}
		cfg := &OpenAIConfig{OAuth: token, AccountID: accountID}
		if err := saveOpenAIConfig(cfg); err != nil {
			return fmt.Errorf("could not save oauth credentials: %w", err)
		}
		fmt.Print("\033[32mOpenAI OAuth login successful.\033[0m\n")
		return nil
	case "2":
		token, accountID, err := loginOpenAIOAuthDevice()
		if err != nil {
			return err
		}
		cfg := &OpenAIConfig{OAuth: token, AccountID: accountID}
		if err := saveOpenAIConfig(cfg); err != nil {
			return fmt.Errorf("could not save oauth credentials: %w", err)
		}
		fmt.Print("\033[32mOpenAI OAuth login successful.\033[0m\n")
		return nil
	case "3":
		return loginOpenAIAPIKey()
	default:
		return fmt.Errorf("invalid selection: %q", choice)
	}
}

func loginOpenAIAPIKey() error {
	fmt.Printf("\n\033[1mOpen this URL in your browser to create an OpenAI API key:\033[0m\n\n")
	fmt.Printf("  \033[36mhttps://platform.openai.com/api-keys\033[0m\n\n")
	fmt.Printf("\033[1mPaste your OpenAI API key here:\033[0m ")

	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return fmt.Errorf("no input")
	}
	key := strings.TrimSpace(scanner.Text())
	if key == "" {
		return fmt.Errorf("no API key provided")
	}
	if !strings.HasPrefix(key, "sk-") {
		return fmt.Errorf("invalid key format (expected key starting with sk-)")
	}

	fmt.Print("Validating key...")
	if err := validateOpenAIKey(key); err != nil {
		fmt.Print("\r\033[K")
		return err
	}
	fmt.Print("\r\033[K")

	cfg := &OpenAIConfig{APIKey: key}
	if err := saveOpenAIConfig(cfg); err != nil {
		return fmt.Errorf("could not save key: %w", err)
	}

	fmt.Print("\033[32mOpenAI API key saved.\033[0m\n")
	return nil
}

func validateOpenAIKey(key string) error {
	req, err := http.NewRequest("GET", "https://api.openai.com/v1/models?limit=1", nil)
	if err != nil {
		return fmt.Errorf("could not create validation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("key validation failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if len(body) > 0 {
		return fmt.Errorf("OpenAI key validation failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return fmt.Errorf("OpenAI key validation failed (%d)", resp.StatusCode)
}

func loginOpenAIOAuthBrowser() (*AuthToken, string, error) {
	verifier, challenge := generatePKCE()
	state := generateState()

	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", openAIOAuthPort))
	if err != nil {
		return nil, "", fmt.Errorf("could not start local callback server on 127.0.0.1:%d: %w", openAIOAuthPort, err)
	}
	defer listener.Close()

	redirect := fmt.Sprintf("http://localhost:%d/auth/callback", openAIOAuthPort)
	authURL := buildOpenAIAuthorizeURL(redirect, challenge, state)
	resultCh := make(chan struct {
		token *AuthToken
		acct  string
		err   error
	}, 1)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			u := r.URL
			if u.Path != "/auth/callback" {
				http.NotFound(w, r)
				return
			}

			if e := u.Query().Get("error"); e != "" {
				desc := u.Query().Get("error_description")
				if desc == "" {
					desc = e
				}
				_, _ = io.WriteString(w, "<h1>Authorization failed</h1><p>"+desc+"</p>")
				resultCh <- struct {
					token *AuthToken
					acct  string
					err   error
				}{err: fmt.Errorf("oauth error: %s", desc)}
				return
			}

			code := strings.TrimSpace(u.Query().Get("code"))
			gotState := strings.TrimSpace(u.Query().Get("state"))
			if code == "" {
				_, _ = io.WriteString(w, "<h1>Authorization failed</h1><p>Missing code.</p>")
				resultCh <- struct {
					token *AuthToken
					acct  string
					err   error
				}{err: fmt.Errorf("missing authorization code")}
				return
			}
			if gotState != state {
				_, _ = io.WriteString(w, "<h1>Authorization failed</h1><p>Invalid state.</p>")
				resultCh <- struct {
					token *AuthToken
					acct  string
					err   error
				}{err: fmt.Errorf("invalid oauth state")}
				return
			}

			token, accountID, err := exchangeOpenAICodeForToken(code, redirect, verifier)
			if err != nil {
				_, _ = io.WriteString(w, "<h1>Authorization failed</h1><p>"+err.Error()+"</p>")
				resultCh <- struct {
					token *AuthToken
					acct  string
					err   error
				}{err: err}
				return
			}

			_, _ = io.WriteString(w, "<h1>Authorization successful</h1><p>You can close this window.</p>")
			resultCh <- struct {
				token *AuthToken
				acct  string
				err   error
			}{token: token, acct: accountID}
		}),
	}

	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Close()

	fmt.Printf("\n\033[1mOpen this URL in your browser to authorize:\033[0m\n\n")
	fmt.Printf("  \033[36m%s\033[0m\n\n", authURL)
	_ = openBrowser(authURL)
	fmt.Print("Waiting for browser callback...")

	select {
	case res := <-resultCh:
		fmt.Print("\r\033[K")
		return res.token, res.acct, res.err
	case <-time.After(5 * time.Minute):
		fmt.Print("\r\033[K")
		return nil, "", fmt.Errorf("oauth callback timeout")
	}
}

func loginOpenAIOAuthDevice() (*AuthToken, string, error) {
	var start struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		Interval     string `json:"interval"`
	}

	startReq, _ := json.Marshal(map[string]string{"client_id": openAICodexClientID})
	resp, err := http.Post(openAIAuthIssuer+"/api/accounts/deviceauth/usercode", "application/json", strings.NewReader(string(startReq)))
	if err != nil {
		return nil, "", fmt.Errorf("failed to start device auth: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("device auth start failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		return nil, "", fmt.Errorf("invalid device auth response: %w", err)
	}
	if start.DeviceAuthID == "" || start.UserCode == "" {
		return nil, "", fmt.Errorf("invalid device auth payload")
	}

	interval := 8 * time.Second
	if i, err := strconv.Atoi(start.Interval); err == nil && i > 0 {
		interval = time.Duration(i)*time.Second + 3*time.Second
	}

	fmt.Printf("\n\033[1mOpen this URL in your browser:\033[0m\n\n  \033[36m%s/codex/device\033[0m\n\n", openAIAuthIssuer)
	fmt.Printf("\033[1mEnter this code:\033[0m \033[36m%s\033[0m\n\n", start.UserCode)
	_ = openBrowser(openAIAuthIssuer + "/codex/device")
	fmt.Print("Waiting for device authorization...")

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		time.Sleep(interval)
		pollReq, _ := json.Marshal(map[string]string{
			"device_auth_id": start.DeviceAuthID,
			"user_code":      start.UserCode,
		})
		pollResp, err := http.Post(openAIAuthIssuer+"/api/accounts/deviceauth/token", "application/json", strings.NewReader(string(pollReq)))
		if err != nil {
			continue
		}
		var payload struct {
			AuthorizationCode string `json:"authorization_code"`
			CodeVerifier      string `json:"code_verifier"`
		}
		if pollResp.StatusCode == 200 {
			_ = json.NewDecoder(pollResp.Body).Decode(&payload)
			pollResp.Body.Close()
			if payload.AuthorizationCode == "" || payload.CodeVerifier == "" {
				fmt.Print("\r\033[K")
				return nil, "", fmt.Errorf("device auth returned invalid authorization payload")
			}
			token, accountID, err := exchangeOpenAICodeForToken(payload.AuthorizationCode, openAIAuthIssuer+"/deviceauth/callback", payload.CodeVerifier)
			fmt.Print("\r\033[K")
			return token, accountID, err
		}
		pollResp.Body.Close()
	}

	fmt.Print("\r\033[K")
	return nil, "", fmt.Errorf("device auth timeout")
}

func buildOpenAIAuthorizeURL(redirectURI, codeChallenge, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", openAICodexClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", "opencode")
	return openAIAuthIssuer + "/oauth/authorize?" + q.Encode()
}

func exchangeOpenAICodeForToken(code, redirectURI, codeVerifier string) (*AuthToken, string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", openAICodexClientID)
	form.Set("code_verifier", codeVerifier)
	return postOpenAIToken(form)
}

func refreshOpenAIToken(refreshToken string) (*AuthToken, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	form.Set("client_id", openAICodexClientID)
	token, _, err := postOpenAIToken(form)
	if err == nil && token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token, err
}

func postOpenAIToken(form url.Values) (*AuthToken, string, error) {
	req, err := http.NewRequest("POST", openAIAuthIssuer+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("oauth token exchange failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, "", fmt.Errorf("invalid oauth token response: %w", err)
	}
	if result.AccessToken == "" {
		return nil, "", fmt.Errorf("missing access token")
	}

	token := &AuthToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
	}
	if result.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Unix() + result.ExpiresIn
	}

	accountID := extractOpenAIAccountID(result.IDToken)
	if accountID == "" {
		accountID = extractOpenAIAccountID(result.AccessToken)
	}

	return token, accountID, nil
}

func extractOpenAIAccountID(jwtToken string) string {
	if jwtToken == "" {
		return ""
	}
	parts := strings.Split(jwtToken, ".")
	if len(parts) != 3 {
		return ""
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		Organizations    []struct {
			ID string `json:"id"`
		} `json:"organizations"`
		OpenAIAuth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(data, &claims); err != nil {
		return ""
	}
	if claims.ChatGPTAccountID != "" {
		return claims.ChatGPTAccountID
	}
	if claims.OpenAIAuth.ChatGPTAccountID != "" {
		return claims.OpenAIAuth.ChatGPTAccountID
	}
	if len(claims.Organizations) > 0 {
		return claims.Organizations[0].ID
	}
	return ""
}

func generateState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func openBrowser(target string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", target).Run()
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", target).Run()
	default:
		return exec.Command("xdg-open", target).Run()
	}
}

func refreshToken(refresh string) (*AuthToken, error) {
	body := fmt.Sprintf(
		`{"grant_type":"refresh_token","refresh_token":"%s","client_id":"%s"}`,
		refresh, clientID,
	)

	resp, err := http.Post(tokenURL, "application/json", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	var expiresAt int64
	if result.ExpiresIn > 0 {
		expiresAt = time.Now().Unix() + result.ExpiresIn
	}

	return &AuthToken{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}

func generatePKCE() (verifier, challenge string) {
	b := make([]byte, 32)
	rand.Read(b)
	verifier = base64.RawURLEncoding.EncodeToString(b)

	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

func urlEncode(s string) string {
	s = strings.ReplaceAll(s, ":", "%3A")
	s = strings.ReplaceAll(s, "/", "%2F")
	s = strings.ReplaceAll(s, " ", "%20")
	return s
}

func configPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "oc", "auth.json"), nil
}

func openAIConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "oc", "openai.json"), nil
}

func loadToken() (*AuthToken, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var token AuthToken
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, err
	}
	return &token, nil
}

func saveToken(token *AuthToken) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(token)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func loadOpenAIConfig() (*OpenAIConfig, error) {
	path, err := openAIConfigPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg OpenAIConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func saveOpenAIConfig(cfg *OpenAIConfig) error {
	path, err := openAIConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
