package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

var (
	ollamaChecked   bool
	ollamaIsRunning bool
)

func ollamaHost() string {
	if h := os.Getenv("OLLAMA_HOST"); h != "" {
		return h
	}
	return "http://localhost:11434"
}

func ollamaModel() string {
	if m := os.Getenv("OLLAMA_MODEL"); m != "" {
		return m
	}
	return "qwen3:14b"
}

func ollamaEmbedModel() string {
	if m := os.Getenv("OLLAMA_EMBED_MODEL"); m != "" {
		return m
	}
	return "nomic-embed-text"
}

// ollamaAvailable checks if Ollama is reachable (cached after first check).
func ollamaAvailable() bool {
	if ollamaChecked {
		return ollamaIsRunning
	}
	ollamaChecked = true
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(ollamaHost() + "/api/tags")
	if err != nil {
		ollamaIsRunning = false
		return false
	}
	resp.Body.Close()
	ollamaIsRunning = resp.StatusCode == 200
	return ollamaIsRunning
}

// callOllama sends a prompt to the local Ollama model and returns the response text.
// Returns "" if Ollama is unavailable or errors.
func callOllama(prompt string, maxTokens int) string {
	body, _ := json.Marshal(map[string]any{
		"model":  ollamaModel(),
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"num_predict": maxTokens,
			"temperature": 0.1,
			"top_p":       0.9,
			"stop":        []string{"\n"},
		},
	})
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(ollamaHost()+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var result struct {
		Response string `json:"response"`
	}
	if json.Unmarshal(data, &result) != nil {
		return ""
	}
	return strings.TrimSpace(result.Response)
}

// callOllamaDeterministic is for short classification tasks where output
// stability matters more than creativity.
func callOllamaDeterministic(prompt string, maxTokens int) string {
	body, _ := json.Marshal(map[string]any{
		"model":  ollamaModel(),
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"num_predict": maxTokens,
			"temperature": 0.0,
			"top_p":       1.0,
			"seed":        1,
		},
	})
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(ollamaHost()+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return ""
	}
	var result struct {
		Response string `json:"response"`
	}
	if json.Unmarshal(data, &result) != nil {
		return ""
	}
	return strings.TrimSpace(result.Response)
}

// ollamaEmbed returns an embedding vector for the given text using Ollama.
// Returns nil if Ollama is unavailable.
func ollamaEmbed(text string) []float64 {
	body, _ := json.Marshal(map[string]any{
		"model": ollamaEmbedModel(),
		"input": text,
	})
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(ollamaHost()+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil
	}
	var result struct {
		Embeddings [][]float64 `json:"embeddings"`
	}
	if json.Unmarshal(data, &result) != nil || len(result.Embeddings) == 0 {
		return nil
	}
	return result.Embeddings[0]
}

func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	d := math.Sqrt(normA) * math.Sqrt(normB)
	if d == 0 {
		return 0
	}
	return dot / d
}

// normalizeTrigger lowercases, removes filler words, sorts remaining words,
// and returns (normalized, sha256-hash-prefix) for template/trace lookup.
func normalizeTrigger(input string) (string, string) {
	var words []string
	for _, w := range strings.Fields(strings.ToLower(input)) {
		if !fillerWords[w] {
			words = append(words, w)
		}
	}
	sort.Strings(words)
	trigger := strings.Join(words, " ")
	h := sha256.Sum256([]byte(trigger))
	triggerID := fmt.Sprintf("%x", h)[:16]
	return trigger, triggerID
}

var fillerWords = map[string]bool{
	"a": true, "an": true, "the": true, "is": true, "it": true,
	"to": true, "for": true, "of": true, "in": true, "on": true,
	"and": true, "or": true, "with": true, "that": true, "this": true,
	"my": true, "me": true, "i": true, "you": true, "we": true,
	"can": true, "do": true, "please": true, "just": true, "some": true,
	"create": true, "set": true, "up": true,
	"new": true, "called": true, "named": true, "using": true, "use": true,
}
