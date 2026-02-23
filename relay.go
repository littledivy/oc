package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/websocket"
)

type RemoteEvent struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

var remotePromptChanBuf = make(chan string, 8)

func pushRemotePrompt(prompt string) {
	remotePromptChanBuf <- prompt
}

func remotePromptCh() <-chan string {
	return remotePromptChanBuf
}

// Relay connection.
var (
	relayConnMu sync.Mutex
	relayConn   *websocket.Conn
	relayURL    string
)

func relayBroadcast(ev RemoteEvent) {
	relayConnMu.Lock()
	c := relayConn
	relayConnMu.Unlock()
	if c == nil {
		return
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	c.Write(data)
}

func handleRelayMessage(msg []byte) {
	var cmd struct {
		Type   string `json:"type"`
		Prompt string `json:"prompt,omitempty"`
	}
	if err := json.Unmarshal(msg, &cmd); err != nil {
		return
	}
	if cmd.Type == "prompt" {
		prompt := strings.TrimSpace(cmd.Prompt)
		if prompt != "" {
			pushRemotePrompt(prompt)
		}
	}
}

func dialRelay(endpoint string) (*websocket.Conn, error) {
	return websocket.Dial(endpoint, "", "http://localhost")
}

func relayEndpoint(relayURL, token, cwd string) string {
	hostname, _ := os.Hostname()
	sessionID := currentSessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	wsURL := strings.TrimRight(relayURL, "/")
	if strings.HasPrefix(wsURL, "http://") {
		wsURL = "ws://" + wsURL[7:]
	} else if strings.HasPrefix(wsURL, "https://") {
		wsURL = "wss://" + wsURL[8:]
	} else if !strings.HasPrefix(wsURL, "ws://") && !strings.HasPrefix(wsURL, "wss://") {
		wsURL = "ws://" + wsURL
	}

	return fmt.Sprintf("%s/register?token=%s&session=%s&cwd=%s&machine=%s",
		wsURL, token, sessionID, cwd, hostname)
}

func relayReadLoop(conn *websocket.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			break
		}
		handleRelayMessage(buf[:n])
	}
}

func relayReconnectLoop(endpoint string) {
	for {
		time.Sleep(5 * time.Second)
		conn, err := dialRelay(endpoint)
		if err != nil {
			continue
		}
		relayConnMu.Lock()
		relayConn = conn
		relayConnMu.Unlock()
		fmt.Fprintf(os.Stderr, "  \033[2mrelay: reconnected\033[0m\n")
		relayReadLoop(conn)
		relayConnMu.Lock()
		relayConn = nil
		relayConnMu.Unlock()
	}
}

// Broadcast helpers.

func remoteBroadcastText(text string) {
	relayBroadcast(RemoteEvent{Type: "text", Data: text})
}

func remoteBroadcastToolCall(name string, input json.RawMessage) {
	relayBroadcast(RemoteEvent{Type: "tool_call", Data: map[string]any{
		"name":  name,
		"input": json.RawMessage(input),
	}})
}

func remoteBroadcastToolResult(name, toolUseID string, result ToolResult) {
	content := result.Content
	if len(content) > 2000 {
		content = content[:2000] + "\n... (truncated)"
	}
	relayBroadcast(RemoteEvent{Type: "tool_result", Data: map[string]any{
		"name":       name,
		"tool_use_id": toolUseID,
		"content":    content,
		"is_error":   result.IsError,
	}})
}

func remoteBroadcastDiff(path, oldContent, newContent string) {
	relayBroadcast(RemoteEvent{Type: "diff", Data: map[string]string{
		"path": path,
		"old":  truncateForRelay(oldContent),
		"new":  truncateForRelay(newContent),
	}})
}

func remoteBroadcastNewFile(path, content string) {
	relayBroadcast(RemoteEvent{Type: "new_file", Data: map[string]string{
		"path":    path,
		"content": truncateForRelay(content),
	}})
}

func truncateForRelay(s string) string {
	if len(s) > 8000 {
		return s[:8000] + "\n... (truncated)"
	}
	return s
}

func remoteBroadcastStatus(status string) {
	relayBroadcast(RemoteEvent{Type: "status", Data: status})
}

func remoteBroadcastUserPrompt(prompt string) {
	relayBroadcast(RemoteEvent{Type: "user_prompt", Data: prompt})
}

func remoteBroadcastStats(stats string) {
	relayBroadcast(RemoteEvent{Type: "stats", Data: stats})
}

func initRemoteUI() {
	if os.Getenv("OC_NO_REMOTE") == "1" {
		return
	}
	relayURL = os.Getenv("OC_RELAY_URL")
	if relayURL == "" {
		relayURL = "https://oc-relay.divy.deno.net"
	}
	relayToken := os.Getenv("OC_RELAY_TOKEN")
	if relayToken == "" {
		return
	}
	cwd, _ := os.Getwd()
	endpoint := relayEndpoint(relayURL, relayToken, cwd)

	// Try initial connection synchronously so the log appears before the prompt.
	conn, err := dialRelay(endpoint)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  \033[2mrelay: %v\033[0m\n", err)
		go relayReconnectLoop(endpoint)
		return
	}
	relayConnMu.Lock()
	relayConn = conn
	relayConnMu.Unlock()
	fmt.Fprintf(os.Stderr, "  \033[2mrelay: %s\033[0m\n", relayURL)

	go func() {
		relayReadLoop(conn)
		relayConnMu.Lock()
		relayConn = nil
		relayConnMu.Unlock()
		relayReconnectLoop(endpoint)
	}()
}
