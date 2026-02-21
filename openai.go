package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

const (
	openAIAPIHost   = "api.openai.com"
	openAIModel     = "gpt-5.3-codex"
	openAICodexHost = "chatgpt.com"
)

func streamChatOpenAI(messages []Message, auth *AuthMethod, systemPrompt string) (*StreamReader, error) {
	reqBody := buildOpenAIRequestBody(messages, systemPrompt)
	url := fmt.Sprintf("https://%s/v1/chat/completions", openAIAPIHost)
	oauthResponses := auth != nil && auth.Token != nil
	if oauthResponses {
		reqBody = buildOpenAIResponsesRequestBody(messages, systemPrompt)
		url = fmt.Sprintf("https://%s/backend-api/codex/responses", openAICodexHost)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	credential, accountID, err := ensureOpenAIToken(auth)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("originator", "oc")
	req.Header.Set("User-Agent", "oc")
	if oauthResponses {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if oauthResponses {
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
		}
		s := bufio.NewScanner(resp.Body)
		s.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
		return &StreamReader{
			scanner: s,
			body:    resp.Body,
			mode:    "openai_responses_sse",
			oa: openAIResponseState{
				itemToCallID: make(map[string]string),
				itemHasDelta: make(map[string]bool),
			},
		}, nil
	}

	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
	}

	events, err := parseOpenAIEvents(body)
	if err != nil {
		return nil, err
	}
	return &StreamReader{events: events}, nil
}

func classifierChatOpenAI(messages []Message, auth *AuthMethod) (string, error) {
	reqBody := map[string]any{
		"model":       openAIModelName(),
		"max_tokens":  256,
		"temperature": 0,
		"messages":    buildOpenAIMessages(messages, ""),
	}
	if auth != nil && auth.Token != nil {
		reqBody = map[string]any{
			"model":        openAIModelName(),
			"instructions": "You are a strict JSON intent classifier. Return only JSON.",
			"stream":       true,
			"store":        false,
			"input":        buildOpenAIResponsesInput(messages, ""),
		}
	}
	data, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("https://%s/v1/chat/completions", openAIAPIHost)
	if auth != nil && auth.Token != nil {
		url = fmt.Sprintf("https://%s/backend-api/codex/responses", openAICodexHost)
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	credential, accountID, err := ensureOpenAIToken(auth)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("originator", "oc")
	req.Header.Set("User-Agent", "oc")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
	}
	var events []StreamEvent
	if auth != nil && auth.Token != nil {
		events = parseOpenAIResponsesSSEBody(body)
	} else {
		var err error
		events, err = parseOpenAIEvents(body)
		if err != nil {
			return "", err
		}
	}
	var text strings.Builder
	var in, out int
	for _, ev := range events {
		if ev.Type == "usage" {
			in += ev.InputTokens
		}
		if ev.Type == "message_delta" {
			out += ev.OutputTokens
		}
		if ev.Type == "text" {
			text.WriteString(ev.Text)
		}
	}
	callbackInputTokens += in
	callbackOutputTokens += out
	callbackAPICalls++
	return text.String(), nil
}

func compactionChatOpenAI(messages []Message, auth *AuthMethod, sysPrompt string) (string, error) {
	if auth != nil && auth.Token != nil {
		reqBody := map[string]any{
			"model":        openAIModelName(),
			"instructions": strings.TrimSpace(sysPrompt),
			"stream":       true,
			"store":        false,
			"input":        buildOpenAIResponsesInput(messages, ""),
			"max_tokens":   4096,
		}
		data, _ := json.Marshal(reqBody)

		url := fmt.Sprintf("https://%s/backend-api/codex/responses", openAICodexHost)
		req, err := http.NewRequest("POST", url, bytes.NewReader(data))
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		credential, accountID, err := ensureOpenAIToken(auth)
		if err != nil {
			return "", err
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		if accountID != "" {
			req.Header.Set("ChatGPT-Account-Id", accountID)
		}
		req.Header.Set("originator", "oc")
		req.Header.Set("User-Agent", "oc")
		req.Header.Set("Accept", "text/event-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
		}
		events := parseOpenAIResponsesSSEBody(body)
		var text strings.Builder
		for _, ev := range events {
			if ev.Type == "text" {
				text.WriteString(ev.Text)
			}
		}
		return strings.TrimSpace(text.String()), nil
	}

	reqBody := map[string]any{
		"model":       openAIModelName(),
		"max_tokens":  4096,
		"temperature": 0,
		"messages":    buildOpenAIMessages(messages, sysPrompt),
	}
	data, _ := json.Marshal(reqBody)

	url := fmt.Sprintf("https://%s/v1/chat/completions", openAIAPIHost)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	credential, accountID, err := ensureOpenAIToken(auth)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	req.Header.Set("originator", "oc")
	req.Header.Set("User-Agent", "oc")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("OpenAI API error %d: %s", resp.StatusCode, string(body))
	}
	events, err := parseOpenAIEvents(body)
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, ev := range events {
		if ev.Type == "text" {
			text.WriteString(ev.Text)
		}
	}
	return strings.TrimSpace(text.String()), nil
}

func parseOpenAIResponsesSSEBody(body []byte) []StreamEvent {
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	state := openAIResponseState{
		itemToCallID: make(map[string]string),
		itemHasDelta: make(map[string]bool),
	}
	var events []StreamEvent
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		events = append(events, parseOpenAIResponsesSSEData(data, &state)...)
	}
	return events
}

func parseOpenAIEvents(body []byte) ([]StreamEvent, error) {
	var chat struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &chat); err == nil && len(chat.Choices) > 0 {
		choice := chat.Choices[0]
		events := []StreamEvent{{Type: "usage", InputTokens: chat.Usage.PromptTokens}}
		if choice.Message.Content != "" {
			events = append(events, StreamEvent{Type: "text", Text: choice.Message.Content})
		}
		for _, tc := range choice.Message.ToolCalls {
			args := strings.TrimSpace(tc.Function.Arguments)
			if args == "" {
				args = "{}"
			}
			events = append(events,
				StreamEvent{Type: "tool_use_start", ToolID: tc.ID, ToolName: tc.Function.Name},
				StreamEvent{Type: "input_json", JSON: args},
				StreamEvent{Type: "block_stop"},
			)
		}
		stopReason := "end_turn"
		if len(choice.Message.ToolCalls) > 0 {
			stopReason = "tool_use"
		} else if strings.TrimSpace(choice.FinishReason) != "" {
			stopReason = choice.FinishReason
		}
		events = append(events, StreamEvent{
			Type:         "message_delta",
			StopReason:   stopReason,
			OutputTokens: chat.Usage.CompletionTokens,
		})
		return events, nil
	}

	var rsp struct {
		Output []struct {
			Type    string `json:"type"`
			CallID  string `json:"call_id"`
			Name    string `json:"name"`
			Args    string `json:"arguments"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &rsp); err != nil {
		return nil, fmt.Errorf("failed to parse OpenAI response: %v", err)
	}

	events := []StreamEvent{{Type: "usage", InputTokens: rsp.Usage.InputTokens}}
	toolCount := 0
	for _, item := range rsp.Output {
		switch item.Type {
		case "message":
			for _, c := range item.Content {
				if c.Type == "output_text" && c.Text != "" {
					events = append(events, StreamEvent{Type: "text", Text: c.Text})
				}
			}
		case "function_call":
			toolCount++
			args := strings.TrimSpace(item.Args)
			if args == "" {
				args = "{}"
			}
			id := item.CallID
			if id == "" {
				id = fmt.Sprintf("call_%d", toolCount)
			}
			events = append(events,
				StreamEvent{Type: "tool_use_start", ToolID: id, ToolName: item.Name},
				StreamEvent{Type: "input_json", JSON: args},
				StreamEvent{Type: "block_stop"},
			)
		}
	}
	stopReason := "end_turn"
	if toolCount > 0 {
		stopReason = "tool_use"
	}
	events = append(events, StreamEvent{
		Type:         "message_delta",
		StopReason:   stopReason,
		OutputTokens: rsp.Usage.OutputTokens,
	})
	return events, nil
}

func parseOpenAIResponsesSSEData(data string, st *openAIResponseState) []StreamEvent {
	var raw map[string]any
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil
	}
	t, _ := raw["type"].(string)

	switch t {
	case "response.output_text.delta":
		if d, ok := raw["delta"].(string); ok && d != "" {
			return []StreamEvent{{Type: "text", Text: d}}
		}
	case "response.output_item.added":
		item, _ := raw["item"].(map[string]any)
		if item == nil {
			return nil
		}
		itemType, _ := item["type"].(string)
		if itemType == "function_call" {
			callID, _ := item["call_id"].(string)
			name, _ := item["name"].(string)
			itemID, _ := item["id"].(string)
			if callID == "" {
				callID = itemID
			}
			if itemID != "" && callID != "" {
				st.itemToCallID[itemID] = callID
			}
			st.hadToolCall = true
			return []StreamEvent{{Type: "tool_use_start", ToolID: callID, ToolName: name}}
		}
	case "response.function_call_arguments.delta":
		itemID, _ := raw["item_id"].(string)
		delta, _ := raw["delta"].(string)
		callID := st.itemToCallID[itemID]
		if callID != "" && delta != "" {
			st.itemHasDelta[itemID] = true
			return []StreamEvent{{Type: "input_json", JSON: delta}}
		}
	case "response.output_item.done":
		item, _ := raw["item"].(map[string]any)
		if item == nil {
			return nil
		}
		itemType, _ := item["type"].(string)
		if itemType == "function_call" {
			callID, _ := item["call_id"].(string)
			args, _ := item["arguments"].(string)
			if callID == "" {
				if itemID, _ := item["id"].(string); itemID != "" {
					callID = st.itemToCallID[itemID]
				}
			}
			if callID == "" {
				callID = "call_unknown"
			}
			var evs []StreamEvent
			itemID, _ := item["id"].(string)
			if !st.itemHasDelta[itemID] && strings.TrimSpace(args) != "" {
				evs = append(evs, StreamEvent{Type: "input_json", JSON: args})
			}
			evs = append(evs, StreamEvent{Type: "block_stop"})
			st.hadToolCall = true
			return evs
		}
	case "response.completed":
		response, _ := raw["response"].(map[string]any)
		var in, out int
		if response != nil {
			usage, _ := response["usage"].(map[string]any)
			if usage != nil {
				if v, ok := usage["input_tokens"].(float64); ok {
					in = int(v)
				}
				if v, ok := usage["output_tokens"].(float64); ok {
					out = int(v)
				}
			}
		}
		stop := "end_turn"
		if st.hadToolCall {
			stop = "tool_use"
		}
		return []StreamEvent{
			{Type: "usage", InputTokens: in},
			{Type: "message_delta", StopReason: stop, OutputTokens: out},
		}
	}

	return nil
}

func buildOpenAIRequestBody(messages []Message, systemPrompt string) []byte {
	req := map[string]any{
		"model":      openAIModelName(),
		"max_tokens": maxTokens,
		"messages":   buildOpenAIMessages(messages, systemPrompt),
		"tools":      buildOpenAITools(),
	}
	data, _ := json.Marshal(req)
	return data
}

func buildOpenAIResponsesRequestBody(messages []Message, systemPrompt string) []byte {
	instructions := strings.TrimSpace(systemPrompt)
	if instructions == "" {
		instructions = systemPrompt
	}
	req := map[string]any{
		"model":        openAIModelName(),
		"instructions": instructions,
		"stream":       true,
		"store":        false,
		"tool_choice":  "auto",
		"input":        buildOpenAIResponsesInput(messages, systemPrompt),
		"tools":        buildOpenAIResponsesTools(),
	}
	if n := envInt("OPENAI_MAX_TOOL_CALLS", 0); n > 0 {
		req["max_tool_calls"] = n
	}
	data, _ := json.Marshal(req)
	return data
}

func openAIModelName() string {
	if model := strings.TrimSpace(os.Getenv("OPENAI_MODEL")); model != "" {
		return model
	}
	return openAIModel
}

func buildOpenAITools() []any {
	return []any{
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "bash",
				"description": "Run terminal commands (build/test/git/runtime). Do not use bash for file search/read/edit operations when dedicated tools exist.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{"type": "string", "description": "The shell command to execute"},
					},
					"required": []string{"command"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "grep",
				"description": "Search text in files using a bounded ripgrep wrapper. Prefer this over running rg/grep via bash for code search. By default ignores .git and node_modules unless explicitly targeting those paths.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern":     map[string]any{"type": "string", "description": "Pattern to search for (literal text or regex)"},
						"path":        map[string]any{"type": "string", "description": "Directory or file path to search (default: .)"},
						"include":     map[string]any{"type": "string", "description": "Optional glob include filter, e.g. *.go or **/*.ts"},
						"max_results": map[string]any{"type": "string", "description": "Optional maximum number of matches to return (default 100, max 200)"},
					},
					"required": []string{"pattern"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "glob",
				"description": "Find files by glob pattern recursively. Prefer this over shell glob expansion for repository discovery.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{"type": "string", "description": "Glob pattern, e.g. **/*.go or src/**/*.ts"},
						"path":    map[string]any{"type": "string", "description": "Directory root to search from (default: .)"},
					},
					"required": []string{"pattern"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "webfetch",
				"description": "Fetch content from an HTTP/HTTPS URL and return readable text.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url":     map[string]any{"type": "string", "description": "URL to fetch"},
						"max_len": map[string]any{"type": "string", "description": "Optional max output characters (default 20000, max 50000)"},
					},
					"required": []string{"url"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "find_symbol",
				"description": "Find symbol definitions/usages using backend auto|lsp|ctags|tree_sitter. Supports bounded results or all matches. By default ignores .git and node_modules unless explicitly targeting those paths.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"symbol":      map[string]any{"type": "string", "description": "Symbol name to find"},
						"path":        map[string]any{"type": "string", "description": "Directory/file to search (default: .)"},
						"backend":     map[string]any{"type": "string", "description": "Search backend: auto, lsp, ctags, tree_sitter"},
						"include":     map[string]any{"type": "string", "description": "Optional glob include filter"},
						"all":         map[string]any{"type": "string", "description": "If true, return all matches (bounded by safety cap)"},
						"max_results": map[string]any{"type": "string", "description": "Maximum matches to return when all=false (default 20, max 500)"},
					},
					"required": []string{"symbol"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "read_files",
				"description": "Read multiple files in one call.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"paths": map[string]any{
							"type":        "array",
							"items":       map[string]any{"type": "string"},
							"description": "Paths to files to read",
						},
					},
					"required": []string{"paths"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "read_file",
				"description": "Read the contents of a file.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":       map[string]any{"type": "string", "description": "Path to the file to read"},
						"start_line": map[string]any{"type": "string", "description": "Optional 1-based start line for partial read"},
						"end_line":   map[string]any{"type": "string", "description": "Optional 1-based end line for partial read"},
						"max_chars":  map[string]any{"type": "string", "description": "Optional max characters to return for this read"},
					},
					"required": []string{"path"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "write_file",
				"description": "Write content to a file, creating it if necessary.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string", "description": "Path to the file to write"},
						"content": map[string]any{"type": "string", "description": "Content to write to the file"},
					},
					"required": []string{"path", "content"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "write_files",
				"description": "Write multiple files in one call.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"files": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"path":    map[string]any{"type": "string"},
									"content": map[string]any{"type": "string"},
								},
								"required": []string{"path", "content"},
							},
							"description": "List of file writes",
						},
					},
					"required": []string{"files"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "edit_file",
				"description": "Edit a file by replacing an exact string match. The old_string must match exactly (including whitespace/indentation). Returns an error with context if no match is found, without requiring an API round-trip.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":       map[string]any{"type": "string", "description": "Path to the file to edit"},
						"old_string": map[string]any{"type": "string", "description": "The exact string to find and replace. Must be unique in the file."},
						"new_string": map[string]any{"type": "string", "description": "The replacement string"},
					},
					"required": []string{"path", "old_string", "new_string"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "list_files",
				"description": "List files and directories in a given path.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path": map[string]any{"type": "string", "description": "Directory path to list"},
					},
					"required": []string{"path"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "todowrite",
				"description": "Replace the current todo list atomically. Input may be either {\"todos\":[...]} or a raw array.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"todos": map[string]any{
							"type": "array",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"id":       map[string]any{"type": "string"},
									"content":  map[string]any{"type": "string"},
									"status":   map[string]any{"type": "string", "description": "pending | in_progress | completed | cancelled"},
									"priority": map[string]any{"type": "string", "description": "high | medium | low"},
								},
								"required": []string{"id", "content", "status", "priority"},
							},
						},
					},
					"required": []string{"todos"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "regex_edit",
				"description": "Apply a regex find-and-replace across multiple files in one call. Use instead of repeated read_file+edit_file for mechanical changes across many files.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern":     map[string]any{"type": "string", "description": "Go-syntax regex pattern to match"},
						"replacement": map[string]any{"type": "string", "description": "Replacement string. Use $1, $2 for capture groups."},
						"glob":        map[string]any{"type": "string", "description": "File glob pattern, e.g. **/*.rs or src/**/*.go"},
						"dry_run":     map[string]any{"type": "string", "description": "If 'true' (default), preview changes. Set to 'false' to apply."},
					},
					"required": []string{"pattern", "replacement", "glob"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "code",
				"description": "Execute TypeScript code with oc.* APIs: read, write, edit, glob, grep, bash, list, ask (LLM). Use for complex multi-file operations.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"code": map[string]any{"type": "string", "description": "TypeScript code to execute with oc.* APIs"},
					},
					"required": []string{"code"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "run_skill",
				"description": buildRunSkillDescription(),
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "Name of the skill to run"},
					},
					"required": []string{"name"},
				},
			},
		},
		map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        "save_skill",
				"description": "Save TypeScript code as a reusable skill.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string", "description": "Short unique name"},
						"description": map[string]any{"type": "string", "description": "What the skill does"},
						"keywords":    map[string]any{"type": "string", "description": "Space-separated keywords for matching"},
						"code":        map[string]any{"type": "string", "description": "TypeScript code"},
					},
					"required": []string{"name", "description", "code"},
				},
			},
		},
	}
}

func buildOpenAIResponsesTools() []any {
	return []any{
		map[string]any{
			"type":        "function",
			"name":        "bash",
			"description": "Run terminal commands (build/test/git/runtime). Do not use bash for file search/read/edit operations when dedicated tools exist.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{"type": "string", "description": "The shell command to execute"},
				},
				"required": []string{"command"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "grep",
			"description": "Search text in files using a bounded ripgrep wrapper. Prefer this over running rg/grep via bash for code search. By default ignores .git and node_modules unless explicitly targeting those paths.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":     map[string]any{"type": "string", "description": "Pattern to search for (literal text or regex)"},
					"path":        map[string]any{"type": "string", "description": "Directory or file path to search (default: .)"},
					"include":     map[string]any{"type": "string", "description": "Optional glob include filter, e.g. *.go or **/*.ts"},
					"max_results": map[string]any{"type": "string", "description": "Optional maximum number of matches to return (default 100, max 200)"},
				},
				"required": []string{"pattern"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "glob",
			"description": "Find files by glob pattern recursively. Prefer this over shell glob expansion for repository discovery.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{"type": "string", "description": "Glob pattern, e.g. **/*.go or src/**/*.ts"},
					"path":    map[string]any{"type": "string", "description": "Directory root to search from (default: .)"},
				},
				"required": []string{"pattern"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "webfetch",
			"description": "Fetch content from an HTTP/HTTPS URL and return readable text.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"url":     map[string]any{"type": "string", "description": "URL to fetch"},
					"max_len": map[string]any{"type": "string", "description": "Optional max output characters (default 20000, max 50000)"},
				},
				"required": []string{"url"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "find_symbol",
			"description": "Find symbol definitions/usages using backend auto|lsp|ctags|tree_sitter. Supports bounded results or all matches. By default ignores .git and node_modules unless explicitly targeting those paths.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"symbol":      map[string]any{"type": "string", "description": "Symbol name to find"},
					"path":        map[string]any{"type": "string", "description": "Directory/file to search (default: .)"},
					"backend":     map[string]any{"type": "string", "description": "Search backend: auto, lsp, ctags, tree_sitter"},
					"include":     map[string]any{"type": "string", "description": "Optional glob include filter"},
					"all":         map[string]any{"type": "string", "description": "If true, return all matches (bounded by safety cap)"},
					"max_results": map[string]any{"type": "string", "description": "Maximum matches to return when all=false (default 20, max 500)"},
				},
				"required": []string{"symbol"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "read_files",
			"description": "Read multiple files in one call.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"paths": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Paths to files to read",
					},
				},
				"required": []string{"paths"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "read_file",
			"description": "Read the contents of a file.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "Path to the file to read"},
					"start_line": map[string]any{"type": "string", "description": "Optional 1-based start line for partial read"},
					"end_line":   map[string]any{"type": "string", "description": "Optional 1-based end line for partial read"},
					"max_chars":  map[string]any{"type": "string", "description": "Optional max characters to return for this read"},
				},
				"required": []string{"path"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "write_file",
			"description": "Write content to a file, creating it if necessary.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string", "description": "Path to the file to write"},
					"content": map[string]any{"type": "string", "description": "Content to write to the file"},
				},
				"required": []string{"path", "content"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "write_files",
			"description": "Write multiple files in one call.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"files": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"path":    map[string]any{"type": "string"},
								"content": map[string]any{"type": "string"},
							},
							"required": []string{"path", "content"},
						},
						"description": "List of file writes",
					},
				},
				"required": []string{"files"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "edit_file",
			"description": "Edit a file by replacing an exact string match. The old_string must match exactly (including whitespace/indentation). Returns an error with context if no match is found, without requiring an API round-trip.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":       map[string]any{"type": "string", "description": "Path to the file to edit"},
					"old_string": map[string]any{"type": "string", "description": "The exact string to find and replace. Must be unique in the file."},
					"new_string": map[string]any{"type": "string", "description": "The replacement string"},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "list_files",
			"description": "List files and directories in a given path.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string", "description": "Directory path to list"},
				},
				"required": []string{"path"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "todowrite",
			"description": "Replace the current todo list atomically. Input may be either {\"todos\":[...]} or a raw array.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"todos": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"id":       map[string]any{"type": "string"},
								"content":  map[string]any{"type": "string"},
								"status":   map[string]any{"type": "string", "description": "pending | in_progress | completed | cancelled"},
								"priority": map[string]any{"type": "string", "description": "high | medium | low"},
							},
							"required": []string{"id", "content", "status", "priority"},
						},
					},
				},
				"required": []string{"todos"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "regex_edit",
			"description": "Apply a regex find-and-replace across multiple files in one call. Use instead of repeated read_file+edit_file for mechanical changes across many files.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern":     map[string]any{"type": "string", "description": "Go-syntax regex pattern to match"},
					"replacement": map[string]any{"type": "string", "description": "Replacement string. Use $1, $2 for capture groups."},
					"glob":        map[string]any{"type": "string", "description": "File glob pattern, e.g. **/*.rs or src/**/*.go"},
					"dry_run":     map[string]any{"type": "string", "description": "If 'true' (default), preview changes. Set to 'false' to apply."},
				},
				"required": []string{"pattern", "replacement", "glob"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "code",
			"description": "Execute TypeScript code with oc.* APIs: read, write, edit, glob, grep, bash, list, ask (LLM). Use for complex multi-file operations.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{"type": "string", "description": "TypeScript code to execute with oc.* APIs"},
				},
				"required": []string{"code"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "run_skill",
			"description": buildRunSkillDescription(),
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string", "description": "Name of the skill to run"},
				},
				"required": []string{"name"},
			},
		},
		map[string]any{
			"type":        "function",
			"name":        "save_skill",
			"description": "Save TypeScript code as a reusable skill.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":        map[string]any{"type": "string", "description": "Short unique name"},
					"description": map[string]any{"type": "string", "description": "What the skill does"},
					"keywords":    map[string]any{"type": "string", "description": "Space-separated keywords for matching"},
					"code":        map[string]any{"type": "string", "description": "TypeScript code"},
				},
				"required": []string{"name", "description", "code"},
			},
		},
	}
}

func buildOpenAIMessages(messages []Message, systemPrompt string) []map[string]any {
	out := make([]map[string]any, 0, len(messages)+1)
	if systemPrompt != "" {
		out = append(out, map[string]any{
			"role":    "system",
			"content": systemPrompt,
		})
	}

	for _, msg := range messages {
		switch c := msg.Content.(type) {
		case string:
			out = append(out, map[string]any{
				"role":    msg.Role,
				"content": c,
			})

		case []ContentBlock:
			if msg.Role == "assistant" {
				var textParts []string
				var toolCalls []map[string]any
				for _, b := range c {
					switch b.Type {
					case "text":
						if b.Text != "" {
							textParts = append(textParts, b.Text)
						}
					case "tool_use":
						args := strings.TrimSpace(string(b.Input))
						if args == "" {
							args = "{}"
						}
						toolCalls = append(toolCalls, map[string]any{
							"id":   b.ID,
							"type": "function",
							"function": map[string]any{
								"name":      b.Name,
								"arguments": args,
							},
						})
					}
				}

				msgObj := map[string]any{
					"role":    "assistant",
					"content": strings.Join(textParts, ""),
				}
				if len(toolCalls) > 0 {
					msgObj["tool_calls"] = toolCalls
				}
				out = append(out, msgObj)
				continue
			}

			if msg.Role == "user" {
				for _, b := range c {
					if b.Type == "tool_result" {
						out = append(out, map[string]any{
							"role":         "tool",
							"tool_call_id": b.ToolUseID,
							"content":      b.Content,
						})
					}
				}
				continue
			}
		}
	}

	return out
}

func buildOpenAIResponsesInput(messages []Message, systemPrompt string) []map[string]any {
	out := make([]map[string]any, 0, len(messages)+1)
	if systemPrompt != "" {
		out = append(out, map[string]any{
			"role":    "developer",
			"content": systemPrompt,
		})
	}

	for _, msg := range messages {
		switch c := msg.Content.(type) {
		case string:
			if msg.Role == "user" {
				out = append(out, map[string]any{
					"role": "user",
					"content": []map[string]any{
						{"type": "input_text", "text": c},
					},
				})
			} else if msg.Role == "assistant" {
				out = append(out, map[string]any{
					"role": "assistant",
					"content": []map[string]any{
						{"type": "output_text", "text": c},
					},
				})
			} else {
				out = append(out, map[string]any{
					"role": msg.Role, "content": c,
				})
			}
		case []ContentBlock:
			switch msg.Role {
			case "assistant":
				for _, b := range c {
					if b.Type == "text" && b.Text != "" {
						out = append(out, map[string]any{
							"role": "assistant",
							"content": []map[string]any{
								{"type": "output_text", "text": b.Text},
							},
						})
						continue
					}
					if b.Type == "tool_use" {
						args := strings.TrimSpace(string(b.Input))
						if args == "" {
							args = "{}"
						}
						id := b.ID
						if id == "" {
							id = "call_unknown"
						}
						out = append(out, map[string]any{
							"type":      "function_call",
							"call_id":   id,
							"name":      b.Name,
							"arguments": args,
						})
					}
				}
			case "user":
				for _, b := range c {
					if b.Type == "tool_result" {
						out = append(out, map[string]any{
							"type":    "function_call_output",
							"call_id": b.ToolUseID,
							"output":  b.Content,
						})
					}
				}
			}
		}
	}

	return out
}
