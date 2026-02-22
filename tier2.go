package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"math/rand"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

func execFindSymbol(symbol, path, backend, include, allRaw, maxResultsRaw string) ToolResult {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ToolResult{Content: "Error: symbol is required", IsError: true}
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		backend = "auto"
	}
	all := parseBoolLike(allRaw)
	maxResults := 20
	if n, ok := parsePositiveInt(maxResultsRaw); ok {
		maxResults = n
	}
	if all {
		maxResults = 1000
	}
	if maxResults > 500 {
		maxResults = 500
	}
	if maxResults < 1 {
		maxResults = 1
	}

	type backendFn func(string, string, string, int) ([]symbolMatch, string, error)
	candidates := []struct {
		name string
		fn   backendFn
	}{
		{"lsp", symbolSearchLSP},
		{"ctags", symbolSearchCtags},
		{"tree_sitter", symbolSearchTreeSitter},
		{"grep_fallback", symbolSearchHeuristic},
	}
	if backend != "auto" {
		found := false
		for _, c := range candidates {
			if c.name == backend {
				candidates = []struct {
					name string
					fn   backendFn
				}{c}
				found = true
				break
			}
		}
		if !found {
			return ToolResult{Content: fmt.Sprintf("Error: unsupported backend %q (use auto|lsp|ctags|tree_sitter)", backend), IsError: true}
		}
	}

	var lastErr error
	for _, c := range candidates {
		matches, detail, err := c.fn(symbol, path, include, maxResults)
		if err != nil {
			lastErr = err
			continue
		}
		if len(matches) == 0 {
			continue
		}
		return formatSymbolMatches(symbol, c.name, detail, matches, maxResults, all)
	}
	if lastErr != nil {
		return ToolResult{Content: fmt.Sprintf("No matches found. Backend error: %v", lastErr), IsError: false}
	}
	return ToolResult{Content: "No matches found.", IsError: false}
}

func symbolSearchHeuristic(symbol, path, include string, maxResults int) ([]symbolMatch, string, error) {
	escaped := regexpQuote(symbol)
	decl := fmt.Sprintf(`(^\s*func\s+(\([^)]+\)\s*)?%s\b)|(^\s*type\s+%s\b)|(^\s*(export\s+)?(async\s+)?function\s+%s\b)|(^\s*(export\s+)?(const|let|var)\s+%s\s*=)|(^\s*(export\s+)?(class|interface|type|enum)\s+%s\b)|(^\s*(pub\s+)?(fn|struct|enum|trait|impl)\s+%s\b)`,
		escaped, escaped, escaped, escaped, escaped, escaped)
	return runRGSearch(decl, path, include, maxResults)
}

func symbolSearchTreeSitter(symbol, path, include string, maxResults int) ([]symbolMatch, string, error) {
	if _, err := exec.LookPath("tree-sitter"); err != nil {
		return nil, "", fmt.Errorf("tree-sitter CLI not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "tree-sitter", "tags", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var matches []symbolMatch
	needle := strings.ToLower(symbol)
	for _, ln := range lines {
		if shouldFilterPathLine(ln, path) {
			continue
		}
		if !strings.Contains(strings.ToLower(ln), needle) {
			continue
		}
		matches = append(matches, symbolMatch{Path: path, Line: "1", Text: ln})
		if len(matches) >= maxResults {
			break
		}
	}
	return matches, "tree-sitter tags", nil
}

func symbolSearchCtags(symbol, path, include string, maxResults int) ([]symbolMatch, string, error) {
	if _, err := exec.LookPath("ctags"); err != nil {
		return nil, "", fmt.Errorf("ctags not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{"-R", "-n", "-f", "-", path}
	if defaultExcludeHeavyDirs(path) {
		args = append([]string{"--exclude=.git", "--exclude=node_modules"}, args...)
	}
	cmd := exec.CommandContext(ctx, "ctags", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, "", err
	}
	lines := strings.Split(string(out), "\n")
	needle := strings.ToLower(symbol)
	var matches []symbolMatch
	for _, ln := range lines {
		if ln == "" || strings.HasPrefix(ln, "!_TAG_") {
			continue
		}
		parts := strings.Split(ln, "\t")
		if len(parts) < 3 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(parts[0]))
		if !strings.Contains(name, needle) {
			continue
		}
		file := strings.TrimSpace(parts[1])
		if shouldFilterPathLine(file, path) {
			continue
		}
		ex := strings.TrimSpace(parts[2])
		line := "1"
		if strings.HasPrefix(ex, "/") && strings.HasSuffix(ex, "/;\"") {
			ex = strings.TrimSuffix(strings.TrimPrefix(ex, "/"), "/;\"")
		}
		if n, ok := parsePositiveInt(strings.TrimSuffix(ex, ";\"")); ok {
			line = strconv.Itoa(n)
		}
		matches = append(matches, symbolMatch{Path: file, Line: line, Text: ln})
		if len(matches) >= maxResults {
			break
		}
	}
	return matches, "ctags", nil
}

func symbolSearchLSP(symbol, path, include string, maxResults int) ([]symbolMatch, string, error) {
	servers := detectLSPServers()
	if len(servers) == 0 {
		return nil, "", fmt.Errorf("no LSP server found (install gopls, rust-analyzer, typescript-language-server, pyright-langserver, or clangd)")
	}
	seen := make(map[string]bool)
	var all []symbolMatch
	var used []string
	for _, server := range servers {
		matches, err := runLSPWorkspaceSymbol(server, path, symbol, maxResults)
		if err != nil {
			continue
		}
		if len(matches) == 0 {
			continue
		}
		used = append(used, server[0])
		for _, m := range matches {
			k := m.Path + ":" + m.Line + ":" + m.Text
			if shouldFilterPathLine(k, path) {
				continue
			}
			if seen[k] {
				continue
			}
			seen[k] = true
			all = append(all, m)
			if len(all) >= maxResults {
				return all, "lsp workspace/symbol", nil
			}
		}
	}
	if len(all) == 0 {
		return nil, "", fmt.Errorf("lsp workspace/symbol returned no matches")
	}
	return all, "lsp workspace/symbol via " + strings.Join(used, ","), nil
}

func detectLSPServers() [][]string {
	candidates := [][]string{
		{"gopls"},
		{"typescript-language-server", "--stdio"},
		{"rust-analyzer"},
		{"pyright-langserver", "--stdio"},
		{"clangd"},
	}
	var out [][]string
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func runLSPWorkspaceSymbol(server []string, rootPath, query string, maxResults int) ([]symbolMatch, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, server[0], server[1:]...)
	cmd.Dir = rootPath
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	br := bufio.NewReader(stdout)
	rootAbs, _ := filepath.Abs(rootPath)
	rootURI := "file://" + filepath.ToSlash(rootAbs)

	if err := writeLSPMessage(stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"processId": nil,
			"rootUri":   rootURI,
			"capabilities": map[string]any{
				"workspace": map[string]any{
					"symbol": map[string]any{
						"dynamicRegistration": false,
					},
				},
			},
		},
	}); err != nil {
		return nil, err
	}
	if _, err := readLSPResponseByID(br, 1); err != nil {
		return nil, err
	}
	_ = writeLSPMessage(stdin, map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	})
	if err := writeLSPMessage(stdin, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "workspace/symbol",
		"params": map[string]any{
			"query": query,
		},
	}); err != nil {
		return nil, err
	}
	resp, err := readLSPResponseByID(br, 2)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(resp, &payload); err != nil {
		return nil, err
	}
	var matches []symbolMatch
	for _, it := range payload.Result {
		name, _ := it["name"].(string)
		loc := extractLSPLocation(it)
		if loc.Path == "" {
			continue
		}
		text := name
		if kind, ok := it["kind"].(float64); ok {
			text = fmt.Sprintf("%s (kind=%d)", name, int(kind))
		}
		matches = append(matches, symbolMatch{
			Path: loc.Path,
			Line: strconv.Itoa(loc.Line),
			Text: text,
		})
		if len(matches) >= maxResults {
			break
		}
	}
	return matches, nil
}

type lspLoc struct {
	Path string
	Line int
}

func extractLSPLocation(item map[string]any) lspLoc {
	if loc, ok := item["location"].(map[string]any); ok {
		if got := parseLSPURIAndLine(loc); got.Path != "" {
			return got
		}
	}
	if got := parseLSPURIAndLine(item); got.Path != "" {
		return got
	}
	return lspLoc{}
}

func parseLSPURIAndLine(obj map[string]any) lspLoc {
	uri, _ := obj["uri"].(string)
	path := lspURIToPath(uri)
	if path == "" {
		return lspLoc{}
	}
	line := 1
	if rg, ok := obj["range"].(map[string]any); ok {
		if st, ok := rg["start"].(map[string]any); ok {
			if ln, ok := st["line"].(float64); ok {
				line = int(ln) + 1
			}
		}
	}
	return lspLoc{Path: path, Line: line}
}

func lspURIToPath(uri string) string {
	if uri == "" {
		return ""
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	if u.Scheme != "file" {
		return ""
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return ""
	}
	if p == "" {
		return ""
	}
	return p
}

func writeLSPMessage(w io.Writer, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body))
	if _, err := io.WriteString(w, header); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

func readLSPResponseByID(br *bufio.Reader, id int) ([]byte, error) {
	target := float64(id)
	for {
		body, err := readLSPMessage(br)
		if err != nil {
			return nil, err
		}
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			continue
		}
		if got, ok := raw["id"].(float64); ok && got == target {
			return body, nil
		}
	}
}

func readLSPMessage(br *bufio.Reader) ([]byte, error) {
	contentLen := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			n := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(line), "content-length:"))
			if v, ok := parsePositiveInt(n); ok {
				contentLen = v
			}
		}
	}
	if contentLen <= 0 {
		return nil, fmt.Errorf("invalid LSP content length")
	}
	body := make([]byte, contentLen)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}

func runRGSearch(pattern, path, include string, maxResults int) ([]symbolMatch, string, error) {
	if _, err := exec.LookPath("rg"); err != nil {
		return nil, "", fmt.Errorf("rg not found")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	args := []string{
		"-nH",
		"--hidden",
		"--no-messages",
		"--color", "never",
		"--field-match-separator=|",
		"-e", pattern,
	}
	if strings.TrimSpace(include) != "" {
		args = append(args, "--glob", include)
	}
	if defaultExcludeHeavyDirs(path) {
		args = append(args, "--glob", "!**/node_modules/**", "--glob", "!**/.git/**")
	}
	args = append(args, path)
	cmd := exec.CommandContext(ctx, "rg", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil, "rg declarations", nil
		}
		return nil, "", err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	matches := make([]symbolMatch, 0, len(lines))
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, "|", 3)
		if len(parts) < 3 {
			continue
		}
		matches = append(matches, symbolMatch{Path: parts[0], Line: parts[1], Text: parts[2]})
		if len(matches) >= maxResults {
			break
		}
	}
	return matches, "rg declarations", nil
}

// denoRuntime is the TypeScript prelude providing oc.* APIs for code mode.
const denoRuntime = `
// oc runtime - file and shell APIs for code mode
const oc = {
  async read(path: string): Promise<string> {
    return await Deno.readTextFile(path);
  },

  async write(path: string, content: string): Promise<void> {
    const dir = path.includes('/') ? path.substring(0, path.lastIndexOf('/')) : null;
    if (dir) await Deno.mkdir(dir, { recursive: true }).catch(() => {});
    await Deno.writeTextFile(path, content);
    console.error('[oc] wrote ' + path);
  },

  async edit(path: string, oldStr: string, newStr: string): Promise<boolean> {
    const content = await Deno.readTextFile(path);
    if (!content.includes(oldStr)) {
      console.error('[oc] edit failed: old_string not found in ' + path);
      return false;
    }
    const newContent = content.replace(oldStr, newStr);
    await Deno.writeTextFile(path, newContent);
    console.error('[oc] edited ' + path);
    return true;
  },

  async glob(pattern: string): Promise<string[]> {
    const cmd = new Deno.Command("find", {
      args: [".", "-type", "f", "-name", pattern.replace("**/", "")],
      stdout: "piped", stderr: "piped"
    });
    const out = await cmd.output();
    const text = new TextDecoder().decode(out.stdout);
    return text.trim().split('\n').filter(Boolean).map(p => p.replace(/^\.\//, ''));
  },

  async grep(pattern: string, path?: string): Promise<string> {
    const args = ["-rn", "--color=never", "--exclude-dir=.git", "--exclude-dir=node_modules", "--exclude-dir=vendor", "--exclude-dir=.oc", pattern, path || "."];
    const cmd = new Deno.Command("grep", { args, stdout: "piped", stderr: "piped" });
    const out = await cmd.output();
    return new TextDecoder().decode(out.stdout);
  },

  async bash(command: string): Promise<{stdout: string, stderr: string, code: number}> {
    const cmd = new Deno.Command("bash", {
      args: ["-c", command],
      stdout: "piped", stderr: "piped"
    });
    const out = await cmd.output();
    return {
      stdout: new TextDecoder().decode(out.stdout),
      stderr: new TextDecoder().decode(out.stderr),
      code: out.code
    };
  },

  async list(path: string = "."): Promise<string[]> {
    const entries: string[] = [];
    for await (const entry of Deno.readDir(path)) {
      entries.push(entry.name + (entry.isDirectory ? "/" : ""));
    }
    return entries.sort();
  },

  async ask(prompt: string): Promise<string> {
    const callbackFile = Deno.env.get("OC_ASK_CALLBACK");
    if (!callbackFile) {
      return "[oc.ask unavailable - no callback configured]";
    }
    const reqFile = callbackFile + ".req";
    const respFile = callbackFile + ".resp";
    await Deno.writeTextFile(reqFile, prompt);
    for (let i = 0; i < 1200; i++) {
      await new Promise(r => setTimeout(r, 100));
      try {
        const resp = await Deno.readTextFile(respFile);
        await Deno.remove(reqFile).catch(() => {});
        await Deno.remove(respFile).catch(() => {});
        return resp;
      } catch { /* not ready yet */ }
    }
    return "[oc.ask timeout]";
  }
};

// Make oc available globally
(globalThis as any).oc = oc;

// User code starts here
`

func execCode(code string) ToolResult {
	if strings.TrimSpace(code) == "" {
		return ToolResult{Content: "Error: code is required", IsError: true}
	}

	denoPath, err := exec.LookPath("deno")
	if err != nil {
		return ToolResult{Content: "Error: deno not found in PATH. Install from https://deno.land", IsError: true}
	}

	tmpFile, err := os.CreateTemp("", "oc-code-*.ts")
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error creating temp file: %v", err), IsError: true}
	}
	defer os.Remove(tmpFile.Name())

	fullCode := denoRuntime + code
	if _, err := tmpFile.WriteString(fullCode); err != nil {
		return ToolResult{Content: fmt.Sprintf("Error writing code: %v", err), IsError: true}
	}
	tmpFile.Close()

	callbackFile := tmpFile.Name() + ".callback"
	defer os.Remove(callbackFile + ".req")
	defer os.Remove(callbackFile + ".resp")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	go handleAskCallbacks(ctx, callbackFile)

	denoArgs := append([]string{"run"}, DenoPermFlags()...)
	denoArgs = append(denoArgs, tmpFile.Name())
	cmd := exec.CommandContext(ctx, denoPath, denoArgs...)
	cmd.Env = append(os.Environ(), "OC_ASK_CALLBACK="+callbackFile)
	cmd.Dir, _ = os.Getwd()

	lb := &liveOutputBuffer{}
	setActiveCmdOutput(lb)
	defer clearActiveCmdOutput()

	var stderr strings.Builder
	cmd.Stdout = lb
	cmd.Stderr = &stderr

	err = cmd.Run()

	output := strings.TrimSpace(lb.String())
	errOutput := strings.TrimSpace(stderr.String())

	var filteredErr []string
	for _, line := range strings.Split(errOutput, "\n") {
		if !strings.HasPrefix(line, "[oc]") && strings.TrimSpace(line) != "" {
			filteredErr = append(filteredErr, line)
		}
	}
	errOutput = strings.Join(filteredErr, "\n")

	if err != nil {
		result := output
		if errOutput != "" {
			result += "\n\nError output:\n" + errOutput
		}
		if result == "" {
			result = fmt.Sprintf("Code execution failed: %v", err)
		}
		return ToolResult{Content: result, IsError: true}
	}

	if output == "" && errOutput != "" {
		output = errOutput
	}
	if output == "" {
		output = "(no output)"
	}

	output = truncateToolOutput("code", output)
	return ToolResult{Content: output, IsError: false}
}

// handleAskCallbacks monitors for oc.ask() requests and calls the LLM
func handleAskCallbacks(ctx context.Context, callbackFile string) {
	reqFile := callbackFile + ".req"
	respFile := callbackFile + ".resp"

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		prompt, err := os.ReadFile(reqFile)
		if err != nil {
			time.Sleep(50 * time.Millisecond)
			continue
		}

		promptStr := strings.TrimSpace(string(prompt))
		if promptStr == "" {
			os.WriteFile(respFile, []byte("[empty prompt]"), 0644)
			os.Remove(reqFile)
			continue
		}

		response, err := codeAskLLM(ctx, promptStr)
		if err != nil {
			os.WriteFile(respFile, []byte(fmt.Sprintf("[error: %v]", err)), 0644)
		} else {
			os.WriteFile(respFile, []byte(response), 0644)
		}
		os.Remove(reqFile)
	}
}

func execRunSkill(name string) ToolResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolResult{Content: "Error: skill name is required", IsError: true}
	}

	if jitEngine == nil || jitEngine.CodeSkills == nil {
		return ToolResult{Content: "Error: no code skills loaded", IsError: true}
	}

	skill := jitEngine.CodeSkills.FindByName(name)
	if skill == nil {
		var available []string
		for _, s := range jitEngine.CodeSkills.Skills {
			available = append(available, s.Name)
		}
		msg := fmt.Sprintf("Error: skill %q not found.", name)
		if len(available) > 0 {
			msg += fmt.Sprintf(" Available skills: %s", strings.Join(available, ", "))
		}
		return ToolResult{Content: msg, IsError: true}
	}

	fmt.Println(dim(fmt.Sprintf("running skill: %s", skill.Name)))
	result := execCode(skill.Code)

	if result.IsError {
		skill.Failures++
	} else {
		skill.Uses++
	}
	jitEngine.CodeSkills.Save()

	return result
}

func execSaveSkill(name, description, keywords, code string) ToolResult {
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolResult{Content: "Error: skill name is required", IsError: true}
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return ToolResult{Content: "Error: skill code is required", IsError: true}
	}

	if jitEngine == nil {
		return ToolResult{Content: "Error: JIT engine not initialized", IsError: true}
	}
	if jitEngine.CodeSkills == nil {
		jitEngine.CodeSkills = &CodeSkillStore{}
	}

	var kw []string
	for _, w := range strings.Fields(strings.ToLower(keywords)) {
		w = strings.TrimSpace(w)
		if w != "" {
			kw = append(kw, w)
		}
	}

	skill := &CodeSkill{
		Name:        name,
		Description: strings.TrimSpace(description),
		Keywords:    kw,
		Code:        code,
		Created:     time.Now(),
	}

	jitEngine.CodeSkills.Add(skill)
	if err := jitEngine.CodeSkills.Save(); err != nil {
		return ToolResult{Content: fmt.Sprintf("Warning: skill saved in memory but failed to persist: %v", err), IsError: false}
	}

	return ToolResult{Content: fmt.Sprintf("Skill %q saved. It can now be invoked with run_skill.", name), IsError: false}
}

// codeAskLLM makes a simple LLM call for oc.ask()
func codeAskLLM(ctx context.Context, prompt string) (string, error) {
	auth, err := getAuth()
	if err != nil || auth == nil {
		return "", fmt.Errorf("no auth configured: %v", err)
	}

	messages := []Message{{Role: "user", Content: prompt}}
	sysPrompt := "You are a helpful assistant. Provide concise, direct answers. Do not use tools - just respond with text."

	var result strings.Builder
	stream, err := streamChat(messages, auth, sysPrompt)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	for {
		select {
		case <-ctx.Done():
			return result.String(), ctx.Err()
		default:
		}

		event, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result.String(), err
		}
		if event.Type == "text" {
			result.WriteString(event.Text)
		}
		if event.Type == "message_stop" {
			break
		}
	}

	return strings.TrimSpace(result.String()), nil
}

// MatchResult from the AI-native cascade.
type MatchResult struct {
	Pattern    *Pattern
	Confidence float64
	Params     map[string]string
	Strategy   string // "embedding", "rag", "intent", "local_llm", "api_classifier", "none"
}

// TraceIndex stores embeddings of past traces for RAG-based matching.
type TraceIndex struct {
	Entries []TraceIndexEntry `json:"entries"`
	path    string
}

// TraceIndexEntry is one indexed trace.
type TraceIndexEntry struct {
	TriggerID string    `json:"trigger_id"`
	Trigger   string    `json:"trigger"`
	Signature string    `json:"signature"`
	Embedding []float64 `json:"embedding"`
	OpSummary string    `json:"op_summary"` // "read_file(calc.go), read_file(calc_test.go), ..."
}

// LoadTraceIndex loads the trace index from disk.
func LoadTraceIndex() *TraceIndex {
	base, err := ocBaseDir()
	if err != nil {
		return &TraceIndex{}
	}
	p := filepath.Join(base, "trace_index.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return &TraceIndex{path: p}
	}
	var ti TraceIndex
	if json.Unmarshal(data, &ti) != nil {
		return &TraceIndex{path: p}
	}
	ti.path = p
	return &ti
}

// Save writes the trace index to disk.
func (ti *TraceIndex) Save() error {
	if ti.path == "" {
		base, err := ocBaseDir()
		if err != nil {
			return err
		}
		ti.path = filepath.Join(base, "trace_index.json")
	}
	data, err := json.MarshalIndent(ti, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ti.path, data, 0o644)
}

// Add indexes a new trace entry. Embeds the trigger if Ollama is available.
func (ti *TraceIndex) Add(trigger string, signature string, ops []IRop) {
	_, trigID := normalizeTrigger(trigger)

	for _, e := range ti.Entries {
		if e.TriggerID == trigID {
			return
		}
	}

	var emb []float64
	if ollamaAvailable() {
		emb = ollamaEmbed(trigger)
	}

	var parts []string
	for _, op := range ops {
		var args map[string]string
		json.Unmarshal(op.Args, &args)
		if p, ok := args["path"]; ok {
			parts = append(parts, fmt.Sprintf("%s(%s)", op.Tool, filepath.Base(p)))
		} else if c, ok := args["command"]; ok {
			cmd := c
			if len(cmd) > 30 {
				cmd = cmd[:30]
			}
			parts = append(parts, fmt.Sprintf("%s(%s)", op.Tool, cmd))
		} else {
			parts = append(parts, op.Tool)
		}
	}

	ti.Entries = append(ti.Entries, TraceIndexEntry{
		TriggerID: trigID,
		Trigger:   trigger,
		Signature: signature,
		Embedding: emb,
		OpSummary: strings.Join(parts, ", "),
	})
}

// FindMatch runs the matching cascade: embedding → local LLM fallback.
// Stops at the first confident match (confidence >= 0.8).
func (j *JIT) FindMatch(prompt string) *MatchResult {
	// Embedding match
	if ollamaAvailable() {
		if result := j.embeddingMatch(prompt); result != nil && result.Confidence >= 0.8 {
			traceLog("[match] embedding hit: pattern=%s conf=%.2f params=%v",
				result.Pattern.ID, result.Confidence, result.Params)
			return result
		}
		traceLog("[match] embedding: no confident match, falling back to cascade")
	}

	if ollamaAvailable() {
		if result := j.matchByLocalLLM(prompt); result != nil && result.Confidence >= 0.8 {
			traceLog("[match] local_llm hit: pattern=%s conf=%.2f params=%v",
				result.Pattern.ID, result.Confidence, result.Params)
			return result
		}
	}

	traceLog("[match] no match found")
	return &MatchResult{Strategy: "none", Confidence: 0}
}

// embeddingMatch embeds the prompt and compares it against all pattern embeddings
// using cosine similarity. Returns the best match if above a minimum threshold.
func (j *JIT) embeddingMatch(prompt string) *MatchResult {
	if len(j.Patterns.Patterns) == 0 {
		return nil
	}
	promptEmb := ollamaEmbed(prompt)
	if promptEmb == nil {
		return nil
	}
	var bestPattern *Pattern
	bestSim := 0.0
	for _, p := range j.Patterns.Patterns {
		if len(p.Embedding) == 0 {
			continue
		}
		sim := cosineSimilarity(promptEmb, p.Embedding)
		if sim > bestSim {
			bestSim = sim
			bestPattern = p
		}
	}
	if bestPattern == nil {
		return nil
	}
	return &MatchResult{
		Pattern:    bestPattern,
		Confidence: bestSim,
		Strategy:   "embedding",
	}
}

// matchByLocalLLM sends a structured prompt to the local Ollama model for classification.
func (j *JIT) matchByLocalLLM(prompt string) *MatchResult {
	if len(j.Patterns.Patterns) == 0 {
		return nil
	}

	var patternDescs strings.Builder
	eligiblePatterns := make(map[string]*Pattern)
	idx := 0
	for _, p := range j.Patterns.Patterns {
		idx++
		eligiblePatterns[p.ID] = p
		patternDescs.WriteString(fmt.Sprintf("%d. ID=%q keywords=[%s] ops=[",
			idx, p.ID, strings.Join(p.Keywords, ", ")))
		for i, op := range p.Ops {
			if i > 0 {
				patternDescs.WriteString(", ")
			}
			patternDescs.WriteString(fmt.Sprintf("%s:%s", op.Kind, op.Tool))
		}
		patternDescs.WriteString("]\n")
	}

	if idx == 0 {
		return nil
	}

	classifyPrompt := fmt.Sprintf(`Given this user request and these known workflow patterns, which pattern matches?
Extract any variable parameters (values that differ between instances).

Request: %q

Patterns:
%s
Respond ONLY in JSON: {"pattern": "<pattern_id>", "confidence": 0.0-1.0, "params": {"key": "value"}} or {"pattern": "none"}`,
		prompt, patternDescs.String())

	resp := callOllama(classifyPrompt, 200)
	if resp == "" {
		return nil
	}

	return parseLLMClassification(resp, eligiblePatterns)
}

// parseLLMClassification parses JSON output from LLM classification (Strategy 3 & 4).
func parseLLMClassification(resp string, patterns map[string]*Pattern) *MatchResult {
	start := strings.Index(resp, "{")
	end := strings.LastIndex(resp, "}")
	if start < 0 || end < 0 || end <= start {
		return nil
	}
	jsonStr := resp[start : end+1]

	var result struct {
		Pattern    string            `json:"pattern"`
		Confidence float64           `json:"confidence"`
		Params     map[string]string `json:"params"`
	}
	if json.Unmarshal([]byte(jsonStr), &result) != nil {
		return nil
	}

	if result.Pattern == "none" || result.Pattern == "" {
		return nil
	}

	p, ok := patterns[result.Pattern]
	if !ok {
		return nil
	}

	if result.Params == nil {
		result.Params = make(map[string]string)
	}

	return &MatchResult{
		Pattern:    p,
		Confidence: result.Confidence,
		Params:     result.Params,
		Strategy:   "local_llm", // caller overrides for api_classifier
	}
}

// NeedTemplate describes what information an intent requires.
// Learned from observing what the LLM reads across similar tasks.
type NeedTemplate struct {
	ID           string     `json:"id"`
	Intent       string     `json:"intent"`        // descriptive: "add_test", "find_refs", "explain_code"
	Needs        []InfoNeed `json:"needs"`         // what information is needed
	Actions      []string   `json:"actions"`       // what the LLM will DO: "write_file", "bash:test"
	TriggerWords []string   `json:"trigger_words"` // words common to all triggers (for entity extraction)
	SeenCount    int        `json:"seen_count"`
}

// InfoNeed describes one piece of information the LLM will need.
type InfoNeed struct {
	Kind      string  `json:"kind"`      // "source_file", "test_file", "grep_results", "file_tree", "file", "related_file"
	Resolver  string  `json:"resolver"`  // "grep", "convention", "read", "list"
	Pattern   string  `json:"pattern"`   // resolver-specific: grep pattern, convention template, path
	Stability float64 `json:"stability"` // how consistently this need appears (0-1)
	AvgTokens int     `json:"avg_tokens"`
}

// ResolvedNeed is a concrete piece of information fetched for the LLM.
type ResolvedNeed struct {
	Kind    string
	Path    string
	Content string
	Tokens  int
}

// ExtractNeeds analyzes a trace and returns the information needs it reveals.
// Includes files touched by writes so a single successful run can prefetch
// those files on the next similar prompt.
func ExtractNeeds(trace *IRTrace) []InfoNeed {
	var needs []InfoNeed
	seen := make(map[string]bool) // dedup by kind+pattern

	for _, op := range trace.Ops {
		if op.Kind != OpRead && op.Kind != OpQuery && op.Kind != OpWrite {
			continue
		}
		need := classifyNeed(op, trace.Ops)
		if need.Kind == "unknown" {
			continue
		}

		key := need.Kind + ":" + need.Resolver + ":" + need.Pattern
		if seen[key] {
			continue
		}
		seen[key] = true

		need.Stability = 1.0 // first observation: fully stable
		need.AvgTokens = estimateTokens(op.Output)
		needs = append(needs, need)
	}
	return needs
}

// ExtractActions returns the action ops (write/exec/assert) from a trace.
func ExtractActions(trace *IRTrace) []string {
	var actions []string
	seen := make(map[string]bool)
	for _, op := range trace.Ops {
		if op.Kind == OpRead || op.Kind == OpQuery {
			continue
		}
		key := op.Tool
		if op.Tool == "bash" {
			if k := canonicalBashActionKey(extractArgCommand(op.Args)); k != "" {
				key = "bash:" + k
			}
		}
		if !seen[key] {
			seen[key] = true
			actions = append(actions, key)
		}
	}
	return actions
}

func canonicalBashActionKey(cmd string) string {
	cmd = strings.TrimSpace(strings.ToLower(cmd))
	if cmd == "" {
		return ""
	}
	cmd = strings.ReplaceAll(cmd, "&&", ";")
	cmd = strings.ReplaceAll(cmd, "||", ";")
	segments := strings.Split(cmd, ";")
	seg := ""
	for _, s := range segments {
		s = strings.TrimSpace(s)
		if s != "" {
			seg = s
		}
	}
	if seg == "" {
		seg = cmd
	}
	fields := strings.Fields(seg)
	if len(fields) == 0 {
		return ""
	}
	base := filepath.Base(fields[0])
	if len(fields) >= 2 {
		return base + " " + fields[1]
	}
	return base
}

// MergeNeeds merges a new set of needs into an existing NeedTemplate.
// Needs that appear in BOTH get stability boosted.
// Needs that appear in only the existing set get stability decayed.
func MergeNeeds(existing *NeedTemplate, newNeeds []InfoNeed, newActions []string, triggerWords []string) {
	existing.SeenCount++

	matched := make(map[int]bool)

	for _, nn := range newNeeds {
		found := false
		for i, en := range existing.Needs {
			if en.Kind == nn.Kind && en.Resolver == nn.Resolver {
				existing.Needs[i].Stability += (1.0 - existing.Needs[i].Stability) * 0.3
				existing.Needs[i].AvgTokens = (existing.Needs[i].AvgTokens + nn.AvgTokens) / 2
				matched[i] = true
				found = true
				break
			}
		}
		if !found {
			nn.Stability = 0.5
			existing.Needs = append(existing.Needs, nn)
		}
	}

	for i := range existing.Needs {
		if !matched[i] {
			existing.Needs[i].Stability *= 0.7
		}
	}

	var kept []InfoNeed
	for _, n := range existing.Needs {
		if n.Stability >= 0.2 {
			kept = append(kept, n)
		}
	}
	existing.Needs = kept

	if len(existing.TriggerWords) == 0 {
		existing.TriggerWords = triggerWords
	} else {
		existing.TriggerWords = intersectWords(existing.TriggerWords, triggerWords)
	}

	actionSet := make(map[string]bool)
	for _, a := range existing.Actions {
		actionSet[a] = true
	}
	for _, a := range newActions {
		if !actionSet[a] {
			existing.Actions = append(existing.Actions, a)
		}
	}
}

// Resolve resolves abstract information needs into concrete data
// by running grep/read against the current project state.
func Resolve(needs []InfoNeed, entity string) []ResolvedNeed {
	var resolved []ResolvedNeed
	totalTokens := 0
	const maxContextTokens = 6000

	for _, need := range needs {
		if need.Stability < 0.6 {
			continue // not confident enough
		}
		if totalTokens >= maxContextTokens {
			break
		}

		switch need.Resolver {
		case "grep":
			r := resolveGrep(need, entity, maxContextTokens-totalTokens)
			for _, rn := range r {
				resolved = append(resolved, rn)
				totalTokens += rn.Tokens
			}

		case "convention":
			r := resolveConvention(need, resolved)
			if r != nil {
				resolved = append(resolved, *r)
				totalTokens += r.Tokens
			}

		case "read":
			content, err := readFileForContext(need.Pattern)
			if err == nil {
				tokens := estimateTokens(content)
				if totalTokens+tokens <= maxContextTokens {
					resolved = append(resolved, ResolvedNeed{
						Kind:    need.Kind,
						Path:    need.Pattern,
						Content: content,
						Tokens:  tokens,
					})
					totalTokens += tokens
				}
			}

		case "list":
			path := need.Pattern
			if path == "" {
				path = "."
			}
			result := execListFiles(path)
			if !result.IsError {
				tokens := estimateTokens(result.Content)
				if totalTokens+tokens <= maxContextTokens {
					resolved = append(resolved, ResolvedNeed{
						Kind:    need.Kind,
						Path:    path,
						Content: result.Content,
						Tokens:  tokens,
					})
					totalTokens += tokens
				}
			}
		}
	}

	return resolved
}

// resolveGrep runs a grep for the entity, then reads matching files.
func resolveGrep(need InfoNeed, entity string, budgetTokens int) []ResolvedNeed {
	pattern := need.Pattern
	if entity != "" {
		if strings.Contains(pattern, "{entity}") {
			pattern = strings.ReplaceAll(pattern, "{entity}", entity)
		} else if pattern != "" {
			words := strings.Fields(pattern)
			if len(words) > 1 {
				for i, w := range words {
					if !isStructuralKeyword(w) {
						words[i] = entity
						break
					}
				}
				pattern = strings.Join(words, " ")
			} else {
				pattern = entity
			}
		} else {
			pattern = entity
		}
	}

	if pattern == "" {
		return nil
	}

	grepCmd := fmt.Sprintf("grep -rl %q . --include='*.go' --include='*.py' --include='*.js' --include='*.ts' --include='*.rs' --include='*.rb' --include='*.java' 2>/dev/null | head -5", pattern)
	out, err := exec.Command("/bin/sh", "-c", grepCmd).Output()
	if err != nil || len(out) == 0 {
		return nil
	}

	var results []ResolvedNeed
	totalTokens := 0
	files := strings.Split(strings.TrimSpace(string(out)), "\n")

	for _, file := range files {
		file = strings.TrimSpace(file)
		if file == "" || strings.HasPrefix(file, ".oc/") {
			continue
		}
		if totalTokens >= budgetTokens {
			break
		}

		ev := SliceFile(file, []string{pattern, entity})
		if ev == nil || len(ev.Slices) == 0 {
			continue
		}

		var content strings.Builder
		for _, s := range ev.Slices {
			content.WriteString(s.Content)
			content.WriteString("\n")
		}

		tokens := estimateTokens(content.String())
		if totalTokens+tokens > budgetTokens {
			continue
		}

		results = append(results, ResolvedNeed{
			Kind:    need.Kind,
			Path:    file,
			Content: content.String(),
			Tokens:  tokens,
		})
		totalTokens += tokens
	}

	return results
}

// resolveConvention finds a file based on a naming convention.
// e.g., "{stem}_test.go" → find the test file for an already-resolved source file.
func resolveConvention(need InfoNeed, alreadyResolved []ResolvedNeed) *ResolvedNeed {
	if !strings.Contains(need.Pattern, "{stem}") {
		content, err := readFileForContext(need.Pattern)
		if err != nil {
			return nil
		}
		return &ResolvedNeed{
			Kind:    need.Kind,
			Path:    need.Pattern,
			Content: content,
			Tokens:  estimateTokens(content),
		}
	}

	for _, r := range alreadyResolved {
		if r.Kind == "source_file" || r.Kind == "file" {
			dir := filepath.Dir(r.Path)
			base := filepath.Base(r.Path)
			ext := filepath.Ext(base)
			stem := strings.TrimSuffix(base, ext)

			testPath := filepath.Join(dir,
				strings.ReplaceAll(
					strings.ReplaceAll(need.Pattern, "{stem}", stem),
					filepath.Ext(need.Pattern), ext))

			content, err := readFileForContext(testPath)
			if err != nil {
				continue
			}
			return &ResolvedNeed{
				Kind:    need.Kind,
				Path:    testPath,
				Content: content,
				Tokens:  estimateTokens(content),
			}
		}
	}
	return nil
}

// readFileForContext reads a file, using SliceFile for large files.
func readFileForContext(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(data)
	lines := strings.Count(content, "\n") + 1
	if lines <= smallFileThreshold {
		return content, nil
	}
	ev := SliceFile(path, nil)
	if ev == nil || len(ev.Slices) == 0 {
		if len(content) > 4000 {
			content = content[:4000] + "\n... (truncated)"
		}
		return content, nil
	}
	var b strings.Builder
	for _, s := range ev.Slices {
		b.WriteString(s.Content)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// AssembleContext packs resolved needs into a context string for the LLM.
func AssembleContext(resolved []ResolvedNeed, userPrompt string) string {
	if len(resolved) == 0 {
		return userPrompt
	}

	var b strings.Builder
	b.WriteString("IMPORTANT: The following context has already been gathered for you. Do NOT re-read or re-grep these files — use the context below instead. You MUST still use edit_file/write_file/bash tools to make actual code changes. NEVER claim you made changes without calling the appropriate tools.\n\n")
	for _, r := range resolved {
		header := r.Kind
		if r.Path != "" {
			header = fmt.Sprintf("%s: %s", r.Kind, r.Path)
		}
		b.WriteString(fmt.Sprintf("=== %s ===\n", header))
		content := r.Content
		if len(content) > 3000 {
			content = content[:3000] + "\n... (truncated)"
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	b.WriteString("User request: " + userPrompt)
	return b.String()
}

// extractEntity pulls the likely target entity from a prompt
// by diffing against the NeedTemplate's known trigger words.
// Preserves original casing for camelCase identifiers like executeTool.
func extractEntity(prompt string, template *NeedTemplate) string {
	originalWords := strings.Fields(prompt)
	templateWords := make(map[string]bool)
	for _, kw := range template.TriggerWords {
		templateWords[kw] = true
	}

	for _, w := range originalWords {
		lower := strings.ToLower(w)
		if fillerWords[lower] {
			continue
		}
		if !templateWords[lower] {
			return w
		}
	}
	return ""
}

// isStructuralKeyword returns true for language keywords that aren't entity names.
func isStructuralKeyword(word string) bool {
	keywords := map[string]bool{
		"func": true, "function": true, "def": true, "class": true,
		"struct": true, "type": true, "interface": true, "trait": true,
		"impl": true, "pub": true, "fn": true, "const": true,
		"var": true, "let": true, "import": true, "from": true,
		"export": true, "module": true, "package": true,
	}
	return keywords[strings.ToLower(word)]
}

// intersectWords returns words present in both slices.
func intersectWords(a, b []string) []string {
	set := make(map[string]bool)
	for _, w := range b {
		set[w] = true
	}
	var result []string
	for _, w := range a {
		if set[w] {
			result = append(result, w)
		}
	}
	return result
}

// NeedTemplateStore manages learned need templates.
type NeedTemplateStore struct {
	Templates []*NeedTemplate `json:"templates"`
}

func LoadNeedTemplates() *NeedTemplateStore {
	base, err := ocBaseDir()
	if err != nil {
		return &NeedTemplateStore{}
	}
	data, err := os.ReadFile(filepath.Join(base, "need_templates.json"))
	if err != nil {
		return &NeedTemplateStore{}
	}
	var store NeedTemplateStore
	if json.Unmarshal(data, &store) != nil {
		return &NeedTemplateStore{}
	}
	return &store
}

func (ns *NeedTemplateStore) Save() error {
	base, err := ocBaseDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(ns, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(base, "need_templates.json"), data, 0o644)
}

// FindByPattern finds a NeedTemplate associated with a pattern ID.
func (ns *NeedTemplateStore) FindByPattern(patternID string) *NeedTemplate {
	for _, t := range ns.Templates {
		if t.ID == patternID {
			return t
		}
	}
	return nil
}

// LearnFromTrace creates or updates a NeedTemplate from a completed trace.
func (ns *NeedTemplateStore) LearnFromTrace(trace *IRTrace, pattern *Pattern) {
	if trace == nil || pattern == nil || len(trace.Ops) < 1 {
		return
	}

	needs := ExtractNeeds(trace)
	actions := ExtractActions(trace)
	if len(needs) == 0 && len(actions) == 0 {
		return
	}
	triggerWords := filterWords(strings.Fields(strings.ToLower(trace.Trigger)))

	existing := ns.FindByPattern(pattern.ID)
	if existing != nil {
		MergeNeeds(existing, needs, actions, triggerWords)
		traceLog("[needs] merged into template %s (%d needs, seen=%d)", existing.ID, len(existing.Needs), existing.SeenCount)
		return
	}

	intent := inferIntent(needs, actions)
	ns.Templates = append(ns.Templates, &NeedTemplate{
		ID:           pattern.ID,
		Intent:       intent,
		Needs:        needs,
		Actions:      actions,
		TriggerWords: triggerWords,
		SeenCount:    1,
	})
	traceLog("[needs] new template %s intent=%q (%d needs)", pattern.ID, intent, len(needs))
}

// inferIntent guesses an intent label from the needs and actions.
func inferIntent(needs []InfoNeed, actions []string) string {
	hasSourceFile := false
	hasTestFile := false
	hasGrepResults := false
	hasWrite := false
	hasTest := false

	for _, n := range needs {
		switch n.Kind {
		case "source_file":
			hasSourceFile = true
		case "test_file":
			hasTestFile = true
		case "grep_results":
			hasGrepResults = true
		}
	}
	for _, a := range actions {
		if a == "write_file" {
			hasWrite = true
		}
		if strings.Contains(a, "test") || strings.Contains(a, "pytest") {
			hasTest = true
		}
	}

	switch {
	case hasTest && !hasWrite:
		return "run_checks"
	case hasSourceFile && hasTestFile && hasWrite && hasTest:
		return "add_test"
	case hasGrepResults && !hasWrite:
		return "find_refs"
	case hasSourceFile && !hasWrite:
		return "explain_code"
	case hasSourceFile && hasWrite:
		return "modify_code"
	case hasGrepResults && hasWrite:
		return "search_and_modify"
	default:
		return "general"
	}
}



// classifyNeed determines what kind of information a read/query op was fetching,
// by examining the op itself and the context of earlier ops in the trace.
func classifyNeed(op IRop, allOps []IRop) InfoNeed {
	switch op.Tool {
	case "read_file":
		return classifyReadNeed(op, allOps)
	case "read_files":
		return InfoNeed{Kind: "file", Resolver: "read", Pattern: ""}
	case "write_file", "write_files", "edit_file":
		path := extractArgPath(op.Args)
		return InfoNeed{
			Kind:     "file",
			Resolver: "read",
			Pattern:  path,
		}
	case "list_files":
		path := extractArgPath(op.Args)
		return InfoNeed{
			Kind:     "file_tree",
			Resolver: "list",
			Pattern:  path,
		}
	case "grep":
		return InfoNeed{
			Kind:     "grep_results",
			Resolver: "grep",
			Pattern:  extractArgPattern(op.Args),
		}
	case "find_symbol":
		return InfoNeed{
			Kind:     "grep_results",
			Resolver: "grep",
			Pattern:  extractArgSymbol(op.Args),
		}
	case "bash":
		if isGrepCommand(op.Args) {
			pattern := extractGrepPattern(op.Args)
			return InfoNeed{
				Kind:     "grep_results",
				Resolver: "grep",
				Pattern:  pattern,
			}
		}
		cmd := extractArgCommand(op.Args)
		if isFindCommand(cmd) {
			return InfoNeed{
				Kind:     "file_search",
				Resolver: "grep",
				Pattern:  extractFindPattern(cmd),
			}
		}
	}
	return InfoNeed{Kind: "unknown", Resolver: "read", Pattern: ""}
}

// classifyReadNeed determines WHY a file was read — was it the source file,
// test file, config, etc.
func classifyReadNeed(op IRop, allOps []IRop) InfoNeed {
	path := extractArgPath(op.Args)
	if path == "" {
		return InfoNeed{Kind: "file", Resolver: "read", Pattern: ""}
	}

	for i := 0; i < op.Index; i++ {
		prev := allOps[i]
		if ((prev.Tool == "bash" && isGrepCommand(prev.Args)) || prev.Tool == "grep") && strings.Contains(prev.Output, path) {
			grepPat := extractGrepPattern(prev.Args)
			if prev.Tool == "grep" {
				grepPat = extractArgPattern(prev.Args)
			}
			return InfoNeed{
				Kind:     "source_file",
				Resolver: "grep",
				Pattern:  grepPat,
			}
		}
	}

	base := filepath.Base(path)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)

	if strings.HasSuffix(stem, "_test") {
		return InfoNeed{
			Kind:     "test_file",
			Resolver: "convention",
			Pattern:  "{stem}_test" + ext,
		}
	}
	if strings.HasPrefix(stem, "test_") {
		return InfoNeed{
			Kind:     "test_file",
			Resolver: "convention",
			Pattern:  "test_{stem}" + ext,
		}
	}
	dir := filepath.Dir(path)
	if strings.Contains(dir, "test") || strings.Contains(dir, "spec") {
		return InfoNeed{
			Kind:     "test_file",
			Resolver: "convention",
			Pattern:  path,
		}
	}

	for i := 0; i < op.Index; i++ {
		prev := allOps[i]
		if prev.Tool == "read_file" {
			prevPath := extractArgPath(prev.Args)
			if prevPath != "" && filepath.Dir(prevPath) == filepath.Dir(path) {
				return InfoNeed{
					Kind:     "related_file",
					Resolver: "read",
					Pattern:  path,
				}
			}
		}
	}

	return InfoNeed{
		Kind:     "file",
		Resolver: "read",
		Pattern:  path,
	}
}

// isGrepCommand checks if a bash command is a grep/rg/ag search.
func isGrepCommand(args json.RawMessage) bool {
	cmd := extractArgCommand(args)
	if cmd == "" {
		return false
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	bin := filepath.Base(fields[0])
	switch bin {
	case "grep", "egrep", "fgrep", "rg", "ag", "ack":
		return true
	}
	return false
}

// extractGrepPattern extracts the search pattern from a grep command.
// e.g., `grep -rn "func Divide" .` → "func Divide"
func extractGrepPattern(args json.RawMessage) string {
	cmd := extractArgCommand(args)
	if cmd == "" {
		return ""
	}
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return ""
	}

	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if strings.HasPrefix(f, "-") {
			continue
		}
		f = strings.Trim(f, `"'`)
		if f == "." || f == "./" || strings.HasPrefix(f, "/") || strings.HasPrefix(f, "./") {
			continue
		}
		if strings.HasPrefix(f, "--include") || strings.HasPrefix(f, "--exclude") {
			continue
		}
		return f
	}
	return ""
}

// isFindCommand checks if a command is a find/fd search.
func isFindCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	bin := filepath.Base(fields[0])
	return bin == "find" || bin == "fd"
}

// extractFindPattern extracts the search pattern from a find command.
func extractFindPattern(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		if f == "-name" || f == "-iname" {
			if i+1 < len(fields) {
				return strings.Trim(fields[i+1], `"'`)
			}
		}
	}
	return ""
}

// extractArgPath extracts the "path" field from tool args.
func extractArgPath(args json.RawMessage) string {
	var a struct {
		Path string `json:"path"`
	}
	json.Unmarshal(args, &a)
	return a.Path
}

// extractArgCommand extracts the "command" field from tool args.
func extractArgCommand(args json.RawMessage) string {
	var a struct {
		Command string `json:"command"`
	}
	json.Unmarshal(args, &a)
	return a.Command
}

func extractArgPattern(args json.RawMessage) string {
	var a struct {
		Pattern string `json:"pattern"`
	}
	json.Unmarshal(args, &a)
	return strings.TrimSpace(a.Pattern)
}

func extractArgSymbol(args json.RawMessage) string {
	var a struct {
		Symbol string `json:"symbol"`
	}
	json.Unmarshal(args, &a)
	return strings.TrimSpace(a.Symbol)
}

// Pattern is a learned tool call sequence from past traces (Tier 2).
// When a similar prompt arrives, we speculatively execute predicted read/query
// ops and pack their results into the LLM context, reducing round trips.
type Pattern struct {
	ID          string         `json:"id"`
	Keywords    []string       `json:"keywords"`
	Ops         []PredictedOp  `json:"ops"`
	Occurrences int            `json:"occurrences"` // times seen
	Successes   int            `json:"successes"`   // times speculation helped
	LastUsed    time.Time      `json:"last_used"`
	Embedding   []float64      `json:"embedding,omitempty"` // embedding of Description
	Signature   map[string]int `json:"signature,omitempty"` // canonical structural signature
	Triggers    []string       `json:"triggers,omitempty"`  // raw user prompts that matched this pattern
	Description string         `json:"description,omitempty"` // rich text for embedding (auto-built)
}

// PredictedOp is one predicted tool call with argument stability tracking.
type PredictedOp struct {
	Tool       string            `json:"tool"`
	Kind       OpKind            `json:"kind"`
	DependsOn  []int             `json:"depends_on"`
	StableArgs map[string]string `json:"stable_args"` // args present in ALL traces with same value
	TotalArgs  int               `json:"total_args"`  // total unique arg keys seen
	SeenCount  int               `json:"seen_count"`  // how many traces included this op
	Stability  float64           `json:"stability"`   // len(StableArgs) / TotalArgs
}

// SpecResult is a single speculative execution result.
type SpecResult struct {
	Op      PredictedOp
	Result  ToolResult
	Elapsed time.Duration
}

// PatternStore manages learned patterns.
type PatternStore struct {
	Patterns []*Pattern `json:"patterns"`
}

func LoadPatterns() *PatternStore {
	base, err := ocBaseDir()
	if err != nil {
		return &PatternStore{}
	}
	dir := filepath.Join(base, "patterns")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &PatternStore{}
	}
	var patterns []*Pattern
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var p Pattern
		if json.Unmarshal(data, &p) == nil {
			patterns = append(patterns, &p)
		}
	}
	return &PatternStore{Patterns: patterns}
}

func (ps *PatternStore) Save() error {
	base, err := ocBaseDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "patterns")
	os.MkdirAll(dir, 0o755)
	for _, p := range ps.Patterns {
		data, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(filepath.Join(dir, p.ID+".json"), data, 0o644)
	}
	return nil
}

// Speculate executes predicted read/query ops and returns results.
// Write and exec ops are NOT executed — only predicted as hints.
// Only executes ops with Stability >= 0.8.
func Speculate(prompt string, pattern *Pattern, recorder *TraceRecorder) []SpecResult {
	var results []SpecResult
	for _, op := range pattern.Ops {
		if op.Kind != OpRead && op.Kind != OpQuery {
			continue // only speculate on safe ops
		}
		if op.Stability < 0.8 {
			continue // not confident enough in args
		}
		if !isSpecOpRelevant(prompt, op) {
			continue // skip low-relevance ops for this prompt
		}

		args := op.StableArgs
		argsJSON, _ := json.Marshal(args)

		start := time.Now()
		result := executeTool(op.Tool, json.RawMessage(argsJSON))
		elapsed := time.Since(start)

		if recorder != nil {
			recorder.RecordEffect(op.Tool, json.RawMessage(argsJSON), result, elapsed)
		}

		results = append(results, SpecResult{Op: op, Result: result, Elapsed: elapsed})
		fmt.Println(dim(fmt.Sprintf("speculate: %s", formatToolCall(op.Tool, json.RawMessage(argsJSON)))))
	}
	return results
}

func isSpecOpRelevant(prompt string, op PredictedOp) bool {
	promptTerms := tokenSet(prompt)
	if len(promptTerms) == 0 {
		return true
	}

	opTerms := tokenSet(strings.Join(specOpTerms(op), " "))
	if len(opTerms) == 0 {
		return true
	}

	for t := range opTerms {
		if promptTerms[t] {
			return true
		}
	}
	return false
}

func specOpTerms(op PredictedOp) []string {
	terms := []string{op.Tool, string(op.Kind)}
	for k, v := range op.StableArgs {
		terms = append(terms, k, v)
		if k == "path" {
			terms = append(terms, filepath.Base(v), filepath.Ext(v))
		}
	}
	sort.Strings(terms)
	return terms
}

func tokenSet(s string) map[string]bool {
	s = strings.ToLower(s)
	out := map[string]bool{}
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '/' || r == '.' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte(' ')
	}
	for _, t := range strings.Fields(b.String()) {
		if len(t) <= 1 {
			continue
		}
		if fillerWords[t] {
			continue
		}
		out[t] = true
	}
	return out
}

// PackSpecResults builds a context string from speculative results.
// Injected into the user message so the LLM sees pre-fetched data.
func PackSpecResults(results []SpecResult) string {
	var b strings.Builder
	b.WriteString("I've already gathered this context for you:\n\n")
	for _, r := range results {
		if r.Result.IsError {
			continue
		}
		argsJSON, _ := json.Marshal(r.Op.StableArgs)
		b.WriteString(fmt.Sprintf("=== %s ===\n", formatToolCall(r.Op.Tool, json.RawMessage(argsJSON))))
		content := r.Result.Content
		if len(content) > 3000 {
			content = content[:3000] + "\n... (truncated)"
		}
		b.WriteString(content)
		b.WriteString("\n\n")
	}
	return b.String()
}

// LearnPattern extracts or updates a pattern from a completed trace.
// Uses structural signature overlap (>=0.8) for merging instead of keyword matching.
func (ps *PatternStore) LearnPattern(trace *IRTrace) {
	if trace == nil || len(trace.Ops) < 1 {
		return
	}

	keywords := strings.Fields(strings.ToLower(trace.Trigger))
	newSig := CanonicalSignature(trace.Ops)

	// Use raw trigger for accumulation; fall back to normalized
	rawTrigger := trace.RawTrigger
	if rawTrigger == "" {
		rawTrigger = trace.Trigger
	}

	for _, p := range ps.Patterns {
		if p.Signature == nil {
			continue
		}
		sigOverlap := SignatureOverlap(newSig, p.Signature)
		embSim := 0.0
		if len(p.Embedding) > 0 {
			if newEmb := ollamaEmbed(trace.Trigger); newEmb != nil {
				embSim = cosineSimilarity(newEmb, p.Embedding)
			}
		}
		if sigOverlap >= 0.8 || embSim >= 0.80 {
			p.Occurrences++
			p.LastUsed = time.Now()
			kwSet := make(map[string]bool)
			for _, k := range p.Keywords {
				kwSet[k] = true
			}
			for _, k := range keywords {
				if !kwSet[k] {
					p.Keywords = append(p.Keywords, k)
				}
			}
			p.addTrigger(rawTrigger)
			p.rebuildDescription()
			p.reembed()
			p.mergeOps(trace.Ops)
			traceLog("[jit] merged into pattern %s (sig=%.2f, emb=%.2f, occ=%d)", p.ID, sigOverlap, embSim, p.Occurrences)
			return
		}
	}

	ops := make([]PredictedOp, len(trace.Ops))
	for i, op := range trace.Ops {
		args := extractArgs(op.Args)
		ops[i] = PredictedOp{
			Tool:       op.Tool,
			Kind:       op.Kind,
			DependsOn:  op.DependsOn,
			StableArgs: args,
			TotalArgs:  len(args),
			SeenCount:  1,
			Stability:  1.0, // first occurrence: all args are "stable"
		}
	}

	_, triggerID := normalizeTrigger(trace.Trigger)
	p := &Pattern{
		ID:          triggerID[:16],
		Keywords:    keywords,
		Ops:         ops,
		Occurrences: 1,
		Successes:   0,
		LastUsed:    time.Now(),
		Signature:   newSig,
		Triggers:    []string{rawTrigger},
	}
	p.rebuildDescription()
	p.reembed()
	ps.Patterns = append(ps.Patterns, p)
	traceLog("[jit] new pattern %s (%d ops)", triggerID[:16], len(ops))
}

// addTrigger appends a raw user prompt to the pattern's trigger history.
// Deduplicates and caps at 50 triggers to bound description size.
func (p *Pattern) addTrigger(raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	for _, t := range p.Triggers {
		if t == raw {
			return
		}
	}
	p.Triggers = append(p.Triggers, raw)
	if len(p.Triggers) > 50 {
		p.Triggers = p.Triggers[len(p.Triggers)-50:]
	}
}

// rebuildDescription builds a rich text block from triggers, keywords, and
// stable args. This text is what gets embedded for semantic matching.
func (p *Pattern) rebuildDescription() {
	var b strings.Builder

	// Past user prompts — the richest signal
	if len(p.Triggers) > 0 {
		b.WriteString("User requests: ")
		b.WriteString(strings.Join(p.Triggers, " | "))
		b.WriteString("\n")
	}

	// Keywords
	if len(p.Keywords) > 0 {
		b.WriteString("Keywords: ")
		b.WriteString(strings.Join(p.Keywords, ", "))
		b.WriteString("\n")
	}

	// Tool operations with stable args
	for _, op := range p.Ops {
		b.WriteString(fmt.Sprintf("Op: %s:%s", op.Kind, op.Tool))
		if len(op.StableArgs) > 0 {
			args := make([]string, 0, len(op.StableArgs))
			for k, v := range op.StableArgs {
				// Skip large values (e.g. file contents) — keep arg names + short values
				if len(v) > 100 {
					args = append(args, k+"=<...>")
				} else {
					args = append(args, k+"="+v)
				}
			}
			b.WriteString(" [")
			b.WriteString(strings.Join(args, ", "))
			b.WriteString("]")
		}
		b.WriteString("\n")
	}

	p.Description = b.String()
}

// reembed updates the pattern's embedding from its Description using Ollama.
// Truncates to ~6000 chars to stay within nomic-embed-text's 2048 token window.
func (p *Pattern) reembed() {
	if !ollamaAvailable() {
		return
	}
	text := p.Description
	if text == "" {
		text = strings.Join(p.Keywords, " ")
	}
	if len(text) > 6000 {
		text = text[:6000]
	}
	if emb := ollamaEmbed(text); emb != nil {
		p.Embedding = emb
	}
}

// mergeOps tracks argument stability across traces.
// For each op match by (kind, tool), intersect args — keep only args
// present in ALL traces with the SAME value.
// Uses positional matching: pattern op i matches new op j by (kind, tool),
// consuming each new op at most once.
func (p *Pattern) mergeOps(newOps []IRop) {
	used := make(map[int]bool)
	for i := range p.Ops {
		bestJ := -1
		bestOverlap := -1
		for j, newOp := range newOps {
			if used[j] || p.Ops[i].Tool != newOp.Tool || p.Ops[i].Kind != newOp.Kind {
				continue
			}
			newArgs := extractArgs(newOp.Args)
			overlap := 0
			for k, v := range p.Ops[i].StableArgs {
				if newArgs[k] == v {
					overlap++
				}
			}
			if overlap > bestOverlap {
				bestOverlap = overlap
				bestJ = j
			}
		}
		if bestJ < 0 {
			continue
		}
		used[bestJ] = true

		p.Ops[i].SeenCount++
		newArgs := extractArgs(newOps[bestJ].Args)

		for k, v := range p.Ops[i].StableArgs {
			if newVal, ok := newArgs[k]; !ok || newVal != v {
				delete(p.Ops[i].StableArgs, k)
			}
		}

		allKeys := make(map[string]bool)
		for k := range p.Ops[i].StableArgs {
			allKeys[k] = true
		}
		for k := range newArgs {
			allKeys[k] = true
		}
		p.Ops[i].TotalArgs = len(allKeys)

		if p.Ops[i].TotalArgs > 0 {
			p.Ops[i].Stability = float64(len(p.Ops[i].StableArgs)) / float64(p.Ops[i].TotalArgs)
		} else {
			p.Ops[i].Stability = 0
		}
	}
}

// extractArgs parses JSON args into a string map.
func extractArgs(raw json.RawMessage) map[string]string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	result := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			result[k] = s
		} else {
			b, _ := json.Marshal(v)
			result[k] = string(b)
		}
	}
	return result
}

// FindByID returns the pattern with the given ID, or nil if not found.
func (ps *PatternStore) FindByID(id string) *Pattern {
	for _, p := range ps.Patterns {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// RebuildAllDescriptions rebuilds the Description field on every pattern.
func (ps *PatternStore) RebuildAllDescriptions() {
	for _, p := range ps.Patterns {
		p.rebuildDescription()
	}
}

// filterWords removes filler/stop words from a word list.
func filterWords(words []string) []string {
	var out []string
	for _, w := range words {
		if !fillerWords[w] {
			out = append(out, w)
		}
	}
	return out
}

// wordOverlap returns Jaccard similarity between two word sets.
func wordOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	bSet := make(map[string]bool)
	for _, w := range b {
		bSet[w] = true
	}
	matches := 0
	for _, w := range a {
		if bSet[w] {
			matches++
		}
	}
	total := len(a) + len(b) - matches
	if total == 0 {
		return 0
	}
	return float64(matches) / float64(total)
}

// Slice represents a relevant portion of a file.
type Slice struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
	Reason    string `json:"reason"` // "error_span", "function_def", "import_block", "full_file"
}

// Evidence holds the collected slices for a context window.
type Evidence struct {
	Slices    []Slice `json:"slices"`
	TokenEst  int     `json:"token_est"`
	MaxTokens int     `json:"max_tokens"`
}

const maxTokensPerFile = 2000
const smallFileThreshold = 100

// SliceFile extracts relevant portions of a file.
// Never sends the full file if large — returns outline + targeted slices.
func SliceFile(path string, focus []string) *Evidence {
	data, err := os.ReadFile(path)
	if err != nil {
		return &Evidence{MaxTokens: maxTokensPerFile}
	}

	lines := strings.Split(string(data), "\n")

	if len(lines) <= smallFileThreshold {
		content := string(data)
		return &Evidence{
			Slices: []Slice{{
				Path:      path,
				StartLine: 1,
				EndLine:   len(lines),
				Content:   content,
				Reason:    "full_file",
			}},
			TokenEst:  estimateTokens(content),
			MaxTokens: maxTokensPerFile,
		}
	}

	var slices []Slice
	totalTokens := 0

	outline := Outline(path)
	if outline != "" {
		slices = append(slices, Slice{
			Path:      path,
			StartLine: 0,
			EndLine:   0,
			Content:   outline,
			Reason:    "outline",
		})
		totalTokens += estimateTokens(outline)
	}

	for _, keyword := range focus {
		if totalTokens >= maxTokensPerFile {
			break
		}
		for i, line := range lines {
			if totalTokens >= maxTokensPerFile {
				break
			}
			if strings.Contains(line, keyword) {
				start := i - 5
				if start < 0 {
					start = 0
				}
				end := i + 15
				if end > len(lines) {
					end = len(lines)
				}

				if overlapsExisting(slices, path, start+1, end) {
					continue
				}

				content := strings.Join(lines[start:end], "\n")
				tokens := estimateTokens(content)
				if totalTokens+tokens > maxTokensPerFile {
					continue
				}

				slices = append(slices, Slice{
					Path:      path,
					StartLine: start + 1,
					EndLine:   end,
					Content:   content,
					Reason:    "focus:" + keyword,
				})
				totalTokens += tokens
			}
		}
	}

	return &Evidence{
		Slices:    slices,
		TokenEst:  totalTokens,
		MaxTokens: maxTokensPerFile,
	}
}

// Outline returns function/type/class signatures without bodies.
func Outline(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var outline strings.Builder
	scanner := bufio.NewScanner(f)
	lineNum := 0

	ext := strings.TrimPrefix(filepath.Ext(path), ".")

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if isSignatureLine(trimmed, ext) {
			outline.WriteString(line)
			outline.WriteByte('\n')
		}
	}

	return outline.String()
}

// isSignatureLine returns true if the line looks like a function/type/class signature.
func isSignatureLine(line, ext string) bool {
	switch ext {
	case "go":
		return strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "type ") ||
			strings.HasPrefix(line, "package ") ||
			strings.HasPrefix(line, "import ")
	case "py":
		return strings.HasPrefix(line, "def ") ||
			strings.HasPrefix(line, "class ") ||
			strings.HasPrefix(line, "import ") ||
			strings.HasPrefix(line, "from ")
	case "rs":
		return strings.HasPrefix(line, "fn ") ||
			strings.HasPrefix(line, "pub fn ") ||
			strings.HasPrefix(line, "struct ") ||
			strings.HasPrefix(line, "pub struct ") ||
			strings.HasPrefix(line, "impl ") ||
			strings.HasPrefix(line, "trait ") ||
			strings.HasPrefix(line, "enum ") ||
			strings.HasPrefix(line, "use ") ||
			strings.HasPrefix(line, "mod ")
	case "js", "ts", "jsx", "tsx":
		return strings.HasPrefix(line, "function ") ||
			strings.HasPrefix(line, "export ") ||
			strings.HasPrefix(line, "class ") ||
			strings.HasPrefix(line, "import ") ||
			strings.HasPrefix(line, "const ") ||
			strings.HasPrefix(line, "interface ")
	case "rb":
		return strings.HasPrefix(line, "def ") ||
			strings.HasPrefix(line, "class ") ||
			strings.HasPrefix(line, "module ") ||
			strings.HasPrefix(line, "require ")
	case "java":
		return strings.Contains(line, "class ") ||
			strings.Contains(line, "interface ") ||
			(strings.Contains(line, "(") && !strings.HasPrefix(line, "//") && !strings.HasPrefix(line, "*"))
	default:
		return strings.HasPrefix(line, "func ") ||
			strings.HasPrefix(line, "def ") ||
			strings.HasPrefix(line, "class ") ||
			strings.HasPrefix(line, "function ")
	}
}

// estimateTokens roughly estimates token count (~4 chars per token).
func estimateTokens(s string) int {
	return (len(s) + 3) / 4
}

func overlapsExisting(slices []Slice, path string, start, end int) bool {
	for _, s := range slices {
		if s.Path == path && s.StartLine > 0 && s.EndLine > 0 {
			if start <= s.EndLine && end >= s.StartLine {
				return true
			}
		}
	}
	return false
}

// sessionContextEntry represents a file recently touched in the session.
type sessionContextEntry struct {
	Path    string
	Content string
	Score   int // number of prompt keywords matched
	Tokens  int
}

// ResolveSessionContext scans the current session messages for recently-touched
// files that match keywords in the current prompt. Returns assembled context
// string ready to prepend to the user message, or empty if nothing relevant.
//
// This avoids discovery round-trips: if the user says "make the timer test pass"
// and timer_test.go was recently read/edited, we inject it directly.
func ResolveSessionContext(messages []Message, prompt string) string {
	keywords := extractPromptKeywords(prompt)
	if len(keywords) == 0 {
		return ""
	}

	entries := map[string]*sessionContextEntry{}
	scanned := 0
	for i := len(messages) - 1; i >= 0 && scanned < 40; i-- {
		scanned++
		msg := messages[i]

		switch c := msg.Content.(type) {
		case string:
			continue
		case []ContentBlock:
			for _, block := range c {
				switch block.Type {
				case "tool_use":
					path := extractPathFromToolInput(block.Name, block.Input)
					if path != "" {
						if _, ok := entries[path]; !ok {
							entries[path] = &sessionContextEntry{Path: path}
						}
					}
				case "tool_result":
					extractPathsFromResult(block.Content, entries)
				}
			}
		}
	}

	if len(entries) == 0 {
		return ""
	}

	for _, entry := range entries {
		entry.Score = scorePathMatch(entry.Path, keywords)
	}

	ranked := make([]*sessionContextEntry, 0, len(entries))
	for _, e := range entries {
		if e.Score > 0 {
			ranked = append(ranked, e)
		}
	}
	if len(ranked) == 0 {
		return ""
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].Score > ranked[j].Score })

	const maxTokens = 6000
	var resolved []ResolvedNeed
	totalTokens := 0

	for _, entry := range ranked {
		if totalTokens >= maxTokens {
			break
		}
		if len(resolved) >= 4 {
			break
		}

		content, err := readSessionContextFile(entry.Path)
		if err != nil {
			continue
		}
		tokens := estimateTokens(content)
		if totalTokens+tokens > maxTokens {
			continue
		}

		resolved = append(resolved, ResolvedNeed{
			Kind:    "session_context",
			Path:    entry.Path,
			Content: content,
			Tokens:  tokens,
		})
		totalTokens += tokens
	}

	if len(resolved) == 0 {
		return ""
	}

	traceLog("[session_context] injected %d files (%d tokens) from session history", len(resolved), totalTokens)
	return AssembleContext(resolved, prompt)
}

// extractPromptKeywords pulls meaningful words from the prompt for matching.
func extractPromptKeywords(prompt string) []string {
	words := strings.Fields(strings.ToLower(prompt))
	var keywords []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'`()[]{}#")
		if len(w) < 3 {
			continue
		}
		if fillerWords[w] {
			continue
		}
		if sessionContextStopWords[w] {
			continue
		}
		keywords = append(keywords, w)
	}
	return keywords
}

var sessionContextStopWords = map[string]bool{
	"file": true, "files": true, "code": true, "function": true,
	"error": true, "fix": true, "add": true, "remove": true,
	"change": true, "update": true, "write": true, "read": true,
	"run": true, "test": true, "tests": true, "pass": true, "fail": true,
	"make": true, "should": true, "need": true, "want": true,
	"look": true, "check": true, "see": true, "try": true,
	"all": true, "now": true, "still": true, "also": true,
}

// scorePathMatch scores how well a file path matches the prompt keywords.
func scorePathMatch(path string, keywords []string) int {
	lower := strings.ToLower(path)
	base := strings.ToLower(filepath.Base(path))
	parts := strings.FieldsFunc(base, func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == '/'
	})

	score := 0
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			score += 3
		}
		for _, part := range parts {
			if part == kw {
				score += 5 // exact part match is strongest
			} else if strings.Contains(part, kw) || strings.Contains(kw, part) {
				score += 2
			}
		}
	}
	return score
}

// extractPathFromToolInput extracts a file path from a tool_use input.
func extractPathFromToolInput(toolName string, input json.RawMessage) string {
	switch toolName {
	case "read_file", "write_file", "edit_file":
		var args struct {
			Path string `json:"path"`
		}
		json.Unmarshal(input, &args)
		return args.Path
	case "read_files":
		var args struct {
			Paths []string `json:"paths"`
		}
		json.Unmarshal(input, &args)
		if len(args.Paths) > 0 {
			return args.Paths[0] // just first for scoring
		}
	case "write_files":
		var args struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		json.Unmarshal(input, &args)
		if len(args.Files) > 0 {
			return args.Files[0].Path
		}
	}
	return ""
}

// extractPathsFromResult finds file paths mentioned in tool result content.
func extractPathsFromResult(content string, entries map[string]*sessionContextEntry) {
	for _, word := range strings.Fields(content) {
		word = strings.Trim(word, ".,;:!?\"'`()[]{}#")
		if !strings.Contains(word, ".") {
			continue
		}
		ext := filepath.Ext(word)
		if ext == "" {
			continue
		}
		switch ext {
		case ".go", ".py", ".js", ".ts", ".tsx", ".jsx", ".rs", ".rb", ".java",
			".c", ".cpp", ".h", ".hpp", ".cs", ".swift", ".kt", ".lua",
			".yaml", ".yml", ".json", ".toml", ".md", ".sh":
		default:
			continue
		}
		path := strings.TrimPrefix(word, "./")
		if _, ok := entries[path]; !ok {
			entries[path] = &sessionContextEntry{Path: path}
		}
	}
}

// readSessionContextFile reads a file for session context injection.
// Uses SliceFile for large files, full read for small ones.
func readSessionContextFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	content := string(data)
	lines := strings.Count(content, "\n") + 1
	if lines <= smallFileThreshold {
		return content, nil
	}
	ev := SliceFile(path, nil)
	if ev == nil || len(ev.Slices) == 0 {
		if len(content) > 4000 {
			content = content[:4000] + "\n... (truncated)"
		}
		return content, nil
	}
	var b strings.Builder
	for _, s := range ev.Slices {
		b.WriteString(s.Content)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// IntentLogRegModel is a multi-head logistic regression model.
// Heads: execute (vs discuss), exclusive (vs multi-intent), command_fit (vs scope mismatch).
type IntentLogRegModel struct {
	Version   int       `json:"version"`
	Feature   string    `json:"feature"` // "hash"
	Dim       int       `json:"dim"`
	Weights   []float64 `json:"weights"`
	Bias      float64   `json:"bias"`
	Samples   int       `json:"samples"`
	TrainAcc  float64   `json:"train_acc"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updated_at"`

	ExclusiveWeights []float64 `json:"exclusive_weights,omitempty"`
	ExclusiveBias    float64   `json:"exclusive_bias,omitempty"`
	FitWeights       []float64 `json:"fit_weights,omitempty"`
	FitBias          float64   `json:"fit_bias,omitempty"`

	ValAcc         float64 `json:"val_acc,omitempty"`
	ExclusiveAcc   float64 `json:"exclusive_acc,omitempty"`
	FitAcc         float64 `json:"fit_acc,omitempty"`
	ExecuteSamples int     `json:"execute_samples,omitempty"`
	DiscussSamples int     `json:"discuss_samples,omitempty"`
}

type intentSample struct {
	Prompt    string
	Execute   bool
	Exclusive bool // only meaningful when Execute=true
	ToolNames []string
}

// intentSampleV2 is the richer training sample with multi-head labels.
type intentSampleV2 struct {
	Prompt    string
	Execute   bool
	Exclusive bool
	Fit       bool
	Weight    float64 // sample weight for class balancing
}

var intentModelState = struct {
	sync.RWMutex
	loaded bool
	model  *IntentLogRegModel
}{}

func intentModelPath() (string, error) {
	base, err := ocBaseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "intent_logreg.json"), nil
}

func globalIntentModelPath() string {
	home := strings.TrimSpace(os.Getenv("HOME"))
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".oc", "intent_logreg.json")
}

func loadIntentLogRegModel() *IntentLogRegModel {
	intentModelState.RLock()
	if intentModelState.loaded {
		m := intentModelState.model
		intentModelState.RUnlock()
		return m
	}
	intentModelState.RUnlock()

	intentModelState.Lock()
	defer intentModelState.Unlock()
	if intentModelState.loaded {
		return intentModelState.model
	}
	var paths []string
	if p, err := intentModelPath(); err == nil && strings.TrimSpace(p) != "" {
		paths = append(paths, p)
	}
	if gp := globalIntentModelPath(); gp != "" {
		paths = append(paths, gp)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var m IntentLogRegModel
		if json.Unmarshal(data, &m) != nil || m.Dim <= 0 || len(m.Weights) != m.Dim {
			continue
		}
		if m.Feature == "" {
			m.Feature = "hash"
		}
		intentModelState.model = &m
		intentModelState.loaded = true
		return intentModelState.model
	}
	intentModelState.loaded = true
	return nil
}

func saveIntentLogRegModel(model *IntentLogRegModel) error {
	if model == nil {
		return errors.New("nil model")
	}
	if err := writeIntentLogRegModelFile(model); err != nil {
		return err
	}
	intentModelState.Lock()
	intentModelState.model = model
	intentModelState.loaded = true
	intentModelState.Unlock()
	return nil
}

func writeIntentLogRegModelFile(model *IntentLogRegModel) error {
	path, err := intentModelPath()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func writeGlobalIntentLogRegModelFile(model *IntentLogRegModel) error {
	path := globalIntentModelPath()
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(model, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func predictIntentLogReg(prompt string) (executeProb float64, confidence float64, feature string, ok bool) {
	model := loadIntentLogRegModel()
	if model == nil || model.Dim <= 0 || len(model.Weights) != model.Dim {
		return 0, 0, "", false
	}
	x := intentFeatureVector(strings.TrimSpace(prompt), model)
	if len(x) != model.Dim {
		return 0, 0, "", false
	}
	z := model.Bias
	for i := 0; i < model.Dim; i++ {
		z += model.Weights[i] * x[i]
	}
	p := sigmoid(z)
	conf := math.Abs(p-0.5) * 2 // 0..1
	return p, conf, model.Feature, true
}

// predictExclusiveLogReg predicts whether the prompt is exclusive (single intent).
func predictExclusiveLogReg(prompt string) (exclusive bool, conf float64, ok bool) {
	model := loadIntentLogRegModel()
	if model == nil || len(model.ExclusiveWeights) != model.Dim {
		return false, 0, false
	}
	x := intentFeatureVector(strings.TrimSpace(prompt), model)
	if len(x) != model.Dim {
		return false, 0, false
	}
	z := model.ExclusiveBias
	for i := 0; i < model.Dim; i++ {
		z += model.ExclusiveWeights[i] * x[i]
	}
	p := sigmoid(z)
	return p >= 0.5, math.Abs(p-0.5) * 2, true
}

// predictFitLogReg predicts whether the prompt+command pair is a good fit.
func predictFitLogReg(prompt, command string) (fit bool, conf float64, ok bool) {
	model := loadIntentLogRegModel()
	if model == nil || len(model.FitWeights) != model.Dim {
		return false, 0, false
	}
	combined := prompt + " |CMD| " + command
	x := intentFeatureVector(strings.TrimSpace(combined), model)
	if len(x) != model.Dim {
		return false, 0, false
	}
	z := model.FitBias
	for i := 0; i < model.Dim; i++ {
		z += model.FitWeights[i] * x[i]
	}
	p := sigmoid(z)
	return p >= 0.5, math.Abs(p-0.5) * 2, true
}

func classifyExecutionIntentLogReg(prompt, command string) (bool, bool, bool, float64, bool, string) {
	p, conf, feature, ok := predictIntentLogReg(prompt)
	if !ok {
		return false, false, false, 0, false, "no_model"
	}

	baseThreshold := 0.60
	finalThreshold := 0.55
	if feature == "hash" && loadIntentLogRegModel().Version < 2 {
		baseThreshold = 0.60
		finalThreshold = 0.66
	}
	if conf < baseThreshold {
		return false, false, false, 0, false, fmt.Sprintf("low_prob_conf=%.2f<th=%.2f p=%.2f feature=%s", conf, baseThreshold, p, feature)
	}

	execNow := p >= 0.5

	exclusive := false
	exclusiveConf := 0.5
	if ex, ec, eok := predictExclusiveLogReg(prompt); eok {
		exclusive = ex
		exclusiveConf = ec
	} else {
		lower := strings.ToLower(prompt)
		multiSignals := []string{" and ", " then ", " also ", " plus ", " after that"}
		for _, sig := range multiSignals {
			if strings.Contains(lower, sig) {
				exclusive = false
				exclusiveConf = 0.7
				break
			}
		}
		if exclusiveConf == 0.5 {
			words := len(strings.Fields(prompt))
			if words <= 5 {
				exclusive = true
				exclusiveConf = 0.7
			}
		}
	}

	fit := true
	fitConf := 0.6
	if f, fc, fok := predictFitLogReg(prompt, command); fok {
		fit = f
		fitConf = fc
	} else {
		fit, fitConf = lexicalCommandFit(prompt, command)
	}

	if !execNow {
		exclusive = true
	}

	finalConf := clamp01(conf*0.6 + exclusiveConf*0.2 + fitConf*0.2)
	if finalConf < finalThreshold {
		return false, false, false, 0, false, fmt.Sprintf("low_final_conf=%.2f<th=%.2f p=%.2f feature=%s", finalConf, finalThreshold, p, feature)
	}
	return execNow, exclusive, fit, finalConf, true, "accepted"
}

// lexicalCommandFit checks if the prompt and command are compatible using
// lexical overlap heuristics.
func lexicalCommandFit(prompt, command string) (bool, float64) {
	pt := tokenSet(prompt)
	ct := tokenSet(command)
	if len(ct) == 0 {
		return true, 0.5
	}

	lower := strings.ToLower(prompt)
	scopeRestrictions := []string{"only", "just", "single", "one test", "specific", "package", "file "}
	for _, r := range scopeRestrictions {
		if strings.Contains(lower, r) {
			return false, 0.7
		}
	}

	overlap := 0.0
	for t := range ct {
		if pt[t] {
			overlap++
		}
	}
	ratio := overlap / float64(len(ct))
	return ratio >= 0.3, clamp01(ratio)
}

func trainIntentModelFromClaude(claudeDir string, maxSamples int) (*IntentLogRegModel, error) {
	rawSamples, err := extractIntentSamplesFromClaude(claudeDir, maxSamples)
	if err != nil {
		return nil, err
	}
	if len(rawSamples) < 30 {
		return nil, fmt.Errorf("not enough labeled samples from claude history: %d", len(rawSamples))
	}

	samples := labelSamplesV2(rawSamples)

	samples = append(samples, syntheticExecSamples()...)
	samples = append(samples, syntheticDiscussSamples()...)
	samples = append(samples, syntheticExclusiveSamples()...)

	if len(samples) < 50 {
		return nil, fmt.Errorf("not enough samples after filtering: %d", len(samples))
	}

	dim := 4096
	var fSamples []featureSample
	execCount, discussCount := 0, 0
	for _, s := range samples {
		x := hashPromptFeatures(s.Prompt, dim)
		if len(x) == 0 {
			continue
		}
		fs := featureSample{X: x, Weight: s.Weight}
		if s.Execute {
			fs.Execute = 1
			execCount++
		}
		if s.Exclusive {
			fs.Exclusive = 1
		}
		if s.Fit {
			fs.Fit = 1
		}
		fSamples = append(fSamples, fs)
		if !s.Execute {
			discussCount++
		}
	}

	if len(fSamples) < 50 {
		return nil, fmt.Errorf("not enough feature samples: %d", len(fSamples))
	}

	if execCount > 0 && discussCount > 0 {
		execW := float64(len(fSamples)) / (2 * float64(execCount))
		discW := float64(len(fSamples)) / (2 * float64(discussCount))
		for i := range fSamples {
			if fSamples[i].Execute > 0.5 {
				fSamples[i].Weight *= execW
			} else {
				fSamples[i].Weight *= discW
			}
		}
	}

	rng := rand.New(rand.NewSource(42))
	rng.Shuffle(len(fSamples), func(i, j int) {
		fSamples[i], fSamples[j] = fSamples[j], fSamples[i]
	})

	splitIdx := len(fSamples) * 80 / 100
	if splitIdx < 20 {
		splitIdx = len(fSamples)
	}
	train := fSamples[:splitIdx]
	val := fSamples[splitIdx:]

	wExec, bExec, trainAccExec, valAccExec := trainHead(train, val, dim, func(s featureSample) float64 { return s.Execute }, func(s featureSample) float64 { return s.Weight })

	var exclTrain, exclVal []featureSample
	for _, s := range train {
		if s.Execute > 0.5 {
			exclTrain = append(exclTrain, s)
		}
	}
	for _, s := range val {
		if s.Execute > 0.5 {
			exclVal = append(exclVal, s)
		}
	}
	var wExcl []float64
	var bExcl float64
	var exclAcc float64
	if len(exclTrain) >= 20 {
		wExcl, bExcl, _, exclAcc = trainHead(exclTrain, exclVal, dim, func(s featureSample) float64 { return s.Exclusive }, func(s featureSample) float64 { return 1.0 })
	}

	fitSamples := syntheticFitSamples(dim)
	var fitTrain, fitVal []featureSample
	if len(fitSamples) > 20 {
		rng.Shuffle(len(fitSamples), func(i, j int) { fitSamples[i], fitSamples[j] = fitSamples[j], fitSamples[i] })
		fitSplit := len(fitSamples) * 80 / 100
		fitTrain = fitSamples[:fitSplit]
		fitVal = fitSamples[fitSplit:]
	}
	var wFit []float64
	var bFit, fitAcc float64
	if len(fitTrain) >= 10 {
		wFit, bFit, _, fitAcc = trainHead(fitTrain, fitVal, dim, func(s featureSample) float64 { return s.Fit }, func(s featureSample) float64 { return 1.0 })
	}

	model := &IntentLogRegModel{
		Version:          2,
		Feature:          "hash",
		Dim:              dim,
		Weights:          wExec,
		Bias:             bExec,
		Samples:          len(fSamples),
		TrainAcc:         trainAccExec,
		ValAcc:           valAccExec,
		Source:           claudeDir,
		UpdatedAt:        time.Now(),
		ExclusiveWeights: wExcl,
		ExclusiveBias:    bExcl,
		ExclusiveAcc:     exclAcc,
		FitWeights:       wFit,
		FitBias:          bFit,
		FitAcc:           fitAcc,
		ExecuteSamples:   execCount,
		DiscussSamples:   discussCount,
	}
	return model, nil
}

// trainHead trains a single logistic regression head with SGD + early stopping.
type featureSample struct {
	X         []float64
	Execute   float64
	Exclusive float64
	Fit       float64
	Weight    float64
}

func trainHead(
	train, val []featureSample,
	dim int,
	label func(featureSample) float64,
	weight func(featureSample) float64,
) (w []float64, bias float64, trainAcc float64, valAcc float64) {
	w = make([]float64, dim)
	bias = 0.0
	lr := 0.05
	l2 := 1e-4
	bestValAcc := 0.0
	bestW := make([]float64, dim)
	bestBias := 0.0
	patience := 4
	stale := 0
	rng := rand.New(rand.NewSource(17))

	for epoch := 0; epoch < 30; epoch++ {
		indices := rng.Perm(len(train))

		for _, idx := range indices {
			s := train[idx]
			y := label(s)
			sw := weight(s)
			z := bias
			for j, xv := range s.X {
				z += w[j] * xv
			}
			p := sigmoid(z)
			g := (p - y) * sw
			for j, xv := range s.X {
				w[j] -= lr * (g*xv + l2*w[j])
			}
			bias -= lr * g
		}
		lr *= 0.92

		if len(val) > 0 {
			correct := 0
			for _, s := range val {
				y := label(s)
				z := bias
				for j, xv := range s.X {
					z += w[j] * xv
				}
				p := sigmoid(z)
				pred := 0.0
				if p >= 0.5 {
					pred = 1.0
				}
				if pred == y {
					correct++
				}
			}
			va := float64(correct) / float64(len(val))
			if va > bestValAcc {
				bestValAcc = va
				copy(bestW, w)
				bestBias = bias
				stale = 0
			} else {
				stale++
				if stale >= patience {
					break
				}
			}
		}
	}

	if len(val) > 0 && bestValAcc > 0 {
		copy(w, bestW)
		bias = bestBias
	}

	correct := 0
	for _, s := range train {
		y := label(s)
		z := bias
		for j, xv := range s.X {
			z += w[j] * xv
		}
		p := sigmoid(z)
		pred := 0.0
		if p >= 0.5 {
			pred = 1.0
		}
		if pred == y {
			correct++
		}
	}
	trainAcc = float64(correct) / float64(len(train))
	valAcc = bestValAcc
	return
}

// labelSamplesV2 converts raw claude samples to v2 training samples
// with better labeling and noise filtering.
func labelSamplesV2(raw []intentSample) []intentSampleV2 {
	var out []intentSampleV2
	for _, s := range raw {
		prompt := strings.TrimSpace(s.Prompt)
		if prompt == "" {
			continue
		}

		lower := strings.ToLower(prompt)
		words := strings.Fields(lower)
		if len(words) <= 1 {
			if isNoisePrompt(lower) {
				continue
			}
		}

		if isNoisePrompt(lower) {
			continue
		}

		execute := false
		if s.Execute {
			hasActionTool := false
			for _, t := range s.ToolNames {
				switch t {
				case "bash", "write_file", "write_files", "edit_file",
					"write", "edit": // Claude-format capitalized names (lowercased)
					hasActionTool = true
				}
			}
			if hasActionTool || len(s.ToolNames) == 0 {
				execute = true
			} else {
				if isImperativePrompt(lower) {
					execute = true
				}
			}
		}

		exclusive := true
		if execute {
			multiSignals := []string{" and ", " then ", " also ", " plus ", " after that ", " before ", " show me ", " read ", " explain "}
			for _, sig := range multiSignals {
				if strings.Contains(lower, sig) && len(words) > 4 {
					exclusive = false
					break
				}
			}
		}

		out = append(out, intentSampleV2{
			Prompt:    prompt,
			Execute:   execute,
			Exclusive: exclusive,
			Fit:       true, // default; fit is trained on synthetic data
			Weight:    1.0,
		})
	}
	return out
}

func isNoisePrompt(lower string) bool {
	noise := map[string]bool{
		"hmm": true, "ok": true, "done": true, "yes": true, "no": true,
		"warmup": true, "thanks": true, "thank you": true, "cool": true,
		"nice": true, "great": true, "k": true, "y": true, "n": true,
		"lgtm": true, "ty": true, "thx": true, "nvm": true, "nevermind": true,
		"idk": true, "sure": true, "yep": true, "nope": true, "yea": true,
		"yeah": true, "nah": true, "what": true, "hm": true, "huh": true,
		"oh": true, "ah": true, "restart": true, "continue": true,
	}
	return noise[strings.TrimSpace(lower)]
}

func isImperativePrompt(lower string) bool {
	imperativeVerbs := []string{
		"run ", "build ", "test ", "fix ", "add ", "remove ", "delete ",
		"update ", "change ", "modify ", "refactor ", "implement ",
		"write ", "make ", "create ", "install ", "deploy ", "start ",
		"stop ", "restart ", "exec ", "execute ", "compile ", "lint ",
		"format ", "check ", "verify ", "debug ", "rerun ",
	}
	for _, v := range imperativeVerbs {
		if strings.HasPrefix(lower, v) {
			return true
		}
	}
	cmdPrefixes := []string{
		"go ", "cargo ", "npm ", "yarn ", "pip ", "python ", "make ",
		"docker ", "git ", "kubectl ", "terraform ", "gradle ",
	}
	for _, p := range cmdPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	return false
}

// syntheticExecSamples generates synthetic execute training samples.
func syntheticExecSamples() []intentSampleV2 {
	prompts := []string{
		"run go test", "go test", "go test ./...", "run tests",
		"cargo test", "run cargo test", "pytest", "run pytest",
		"npm test", "npm run test", "make test",
		"run the test suite", "run all tests", "rerun tests",
		"please run tests", "just run tests", "execute the tests",
		"go build", "run go build", "cargo build", "npm run build",
		"make build", "build the project", "compile",
		"go build ./...", "run the build", "rebuild",
		"execute the build", "build it",
		"run go vet", "go vet ./...", "golangci-lint run",
		"cargo clippy", "npm run lint", "make lint",
		"run the linter", "lint", "check the code",
		"run it", "run it again", "rerun", "again",
		"run that again", "retry", "do it again",
		"fix the bug", "fix this error", "add a test for this",
		"implement the feature", "refactor this function",
		"delete the unused import", "remove the dead code",
		"update the dependency", "add error handling",
		"write a test", "create the migration",
		"rename the variable", "extract this into a function",
		"git status", "git diff", "git log", "git add .",
		"docker build .", "docker compose up",
		"pip install -r requirements.txt",
	}
	var out []intentSampleV2
	for _, p := range prompts {
		out = append(out, intentSampleV2{
			Prompt: p, Execute: true, Exclusive: true, Fit: true, Weight: 2.0,
		})
	}
	return out
}

// syntheticDiscussSamples generates synthetic discuss training samples.
func syntheticDiscussSamples() []intentSampleV2 {
	prompts := []string{
		"do tests pass", "did tests pass", "are tests passing",
		"what tests do we have", "should we add more tests",
		"why are tests failing", "which tests are flaky",
		"explain test status", "how do the tests work",
		"does the build work", "what build errors do we have",
		"is the build broken", "why does the build fail",
		"what is this function doing", "explain this code",
		"how does this work", "what does this do",
		"walk me through this", "describe the architecture",
		"what are the dependencies", "how is this structured",
		"can you explain", "what do you think",
		"is this correct", "any suggestions",
		"how well does this scale", "is there a security issue",
		"what are the options", "should we use X or Y",
		"how would you approach this", "what's the best way",
		"what's the tradeoff", "which approach is better",
		"what's the current state", "where are we at",
		"what's left to do", "what broke",
		"how much work is left", "are there any issues",
		"what's the performance like", "any regressions",
		"is this a good pattern", "should we refactor this",
		"is this code safe", "could this cause problems",
		"what's the risk", "is there a race condition",
		"does this handle edge cases", "any memory leaks",
		"explain this", "explain the architecture",
		"describe how this works", "tell me about this code",
		"summarize what this does", "what's happening here",
		"explain the design", "explain the flow",
		"walk me through the logic", "how is this organized",
		"do the tests work", "do tests still pass",
		"did the build succeed", "did it work",
		"does this compile", "does this function handle nulls",
		"did we break anything", "did you fix it",
	}
	var out []intentSampleV2
	for _, p := range prompts {
		out = append(out, intentSampleV2{
			Prompt: p, Execute: false, Exclusive: true, Fit: true, Weight: 2.0,
		})
	}
	return out
}

// syntheticExclusiveSamples generates training samples for the exclusive head.
func syntheticExclusiveSamples() []intentSampleV2 {
	exclusive := []string{
		"run go test", "go build", "cargo test", "npm test",
		"run tests", "build it", "lint the code", "format the code",
		"run pytest", "make test", "make build",
	}
	nonExclusive := []string{
		"show me README and run go test",
		"open the file then run tests",
		"run tests and explain failures",
		"run go test and summarize output",
		"read docs and run test suite",
		"build and show me the errors",
		"fix the bug and run tests",
		"read the config and then build",
		"run tests and also check lint",
		"add the test and then run it",
	}
	var out []intentSampleV2
	for _, p := range exclusive {
		out = append(out, intentSampleV2{
			Prompt: p, Execute: true, Exclusive: true, Fit: true, Weight: 1.5,
		})
	}
	for _, p := range nonExclusive {
		out = append(out, intentSampleV2{
			Prompt: p, Execute: true, Exclusive: false, Fit: true, Weight: 1.5,
		})
	}
	return out
}

// syntheticFitSamples generates prompt+command pairs for the fit head.
func syntheticFitSamples(dim int) []featureSample {
	type pair struct {
		prompt  string
		command string
		fit     bool
	}
	pairs := []pair{
		{"run go test", "go test ./...", true},
		{"run tests", "go test ./...", true},
		{"go test", "go test ./...", true},
		{"run the test suite", "go test ./...", true},
		{"run all tests", "go test ./...", true},
		{"test everything", "go test ./...", true},
		{"execute tests", "go test ./...", true},
		{"rerun tests", "go test ./...", true},
		{"run go build", "go build ./...", true},
		{"build", "go build ./...", true},
		{"build the project", "go build ./...", true},
		{"compile", "go build ./...", true},
		{"cargo test", "cargo test", true},
		{"run cargo test", "cargo test", true},
		{"run cargo build", "cargo build", true},
		{"cargo build", "cargo build", true},
		{"npm test", "npm test", true},
		{"npm run build", "npm run build", true},
		{"make test", "make test", true},
		{"make build", "make build", true},
		{"run pytest", "pytest", true},
		{"pytest", "pytest", true},
		{"run the linter", "golangci-lint run", true},
		{"lint", "golangci-lint run", true},
		{"run lint", "npm run lint", true},
		{"check the code", "cargo clippy", true},

		{"only run 1 test", "go test ./...", false},
		{"run tests for package x", "go test ./...", false},
		{"test just the auth module", "go test ./...", false},
		{"run a single test", "go test ./...", false},
		{"test this file only", "go test ./...", false},
		{"run tests in the api package", "go test ./...", false},
		{"only test the parser", "cargo test", false},
		{"run specific test TestFoo", "go test ./...", false},
		{"test just one function", "pytest", false},
		{"run test for login handler", "npm test", false},
		{"test only utils", "go test ./...", false},
		{"run TestAdd", "go test ./...", false},
		{"just the unit tests", "go test ./...", false},
		{"test the new feature only", "pytest", false},
		{"build only the cli", "go build ./...", false},
		{"compile just main.go", "go build ./...", false},
		{"lint only this file", "golangci-lint run", false},
		{"test -run TestSpecific", "go test ./...", false},
		{"run tests for the auth package only", "npm test", false},
		{"check just the parser module", "cargo clippy", false},
	}

	var out []featureSample
	for _, p := range pairs {
		combined := p.prompt + " |CMD| " + p.command
		x := hashPromptFeatures(combined, dim)
		if len(x) == 0 {
			continue
		}
		fs := featureSample{X: x, Weight: 2.0}
		if p.fit {
			fs.Fit = 1
		}
		out = append(out, fs)
	}
	return out
}

func extractIntentSamplesFromClaude(claudeDir string, maxSamples int) ([]intentSample, error) {
	projectsDir := filepath.Join(claudeDir, "projects")
	var files []string
	err := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	var samples []intentSample

	type record struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"message"`
	}

	for _, f := range files {
		if maxSamples > 0 && len(samples) >= maxSamples {
			break
		}
		fd, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fd)
		sc.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)

		currentUser := ""
		sawToolUse := false
		haveAssistantTurn := false
		var toolNames []string
		flush := func() {
			if currentUser == "" || !haveAssistantTurn {
				return
			}
			samples = append(samples, intentSample{
				Prompt:    currentUser,
				Execute:   sawToolUse,
				ToolNames: toolNames,
			})
		}

		for sc.Scan() {
			if maxSamples > 0 && len(samples) >= maxSamples {
				break
			}
			line := sc.Bytes()
			var r record
			if json.Unmarshal(line, &r) != nil {
				continue
			}
			if r.Type == "user" && strings.ToLower(strings.TrimSpace(r.Message.Role)) == "user" {
				flush()
				currentUser = ""
				sawToolUse = false
				haveAssistantTurn = false
				toolNames = nil

				switch c := r.Message.Content.(type) {
				case string:
					prompt := strings.TrimSpace(c)
					if prompt != "" {
						currentUser = prompt
					}
				}
				continue
			}
			if currentUser == "" || r.Type != "assistant" {
				continue
			}

			hasToolUse := false
			switch c := r.Message.Content.(type) {
			case []any:
				for _, block := range c {
					obj, ok := block.(map[string]any)
					if !ok {
						continue
					}
					bt, _ := obj["type"].(string)
					if bt == "tool_use" {
						hasToolUse = true
						if name, _ := obj["name"].(string); name != "" {
							toolNames = append(toolNames, strings.ToLower(name))
						}
					}
				}
			}
			haveAssistantTurn = true
			if hasToolUse {
				sawToolUse = true
			}
		}
		flush()
		fd.Close()
	}
	return samples, nil
}

func sigmoid(x float64) float64 {
	if x >= 0 {
		z := math.Exp(-x)
		return 1 / (1 + z)
	}
	z := math.Exp(x)
	return z / (1 + z)
}

func runTrainIntentCommand(args []string) error {
	claudeDir := filepath.Join(os.Getenv("HOME"), ".claude")
	maxSamples := 8000
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--claude-dir":
			if i+1 >= len(args) {
				return fmt.Errorf("missing value for --claude-dir")
			}
			claudeDir = args[i+1]
			i++
		case "--max-samples":
			if i+1 >= len(args) {
				return fmt.Errorf("missing value for --max-samples")
			}
			var n int
			if _, err := fmt.Sscanf(args[i+1], "%d", &n); err != nil || n <= 0 {
				return fmt.Errorf("invalid --max-samples: %q", args[i+1])
			}
			maxSamples = n
			i++
		default:
			return fmt.Errorf("unknown arg: %s", args[i])
		}
	}

	fmt.Fprintf(os.Stderr, "training intent model from %s (max_samples=%d)\n", claudeDir, maxSamples)
	model, err := trainIntentModelFromClaude(claudeDir, maxSamples)
	if err != nil {
		return err
	}
	if err := saveIntentLogRegModel(model); err != nil {
		return err
	}
	_ = writeGlobalIntentLogRegModelFile(model)

	fmt.Printf("intent model v%d saved\n", model.Version)
	fmt.Printf("  feature=%s dim=%d samples=%d (exec=%d discuss=%d)\n",
		model.Feature, model.Dim, model.Samples, model.ExecuteSamples, model.DiscussSamples)
	fmt.Printf("  execute head:    train_acc=%.3f val_acc=%.3f\n", model.TrainAcc, model.ValAcc)
	if len(model.ExclusiveWeights) > 0 {
		fmt.Printf("  exclusive head:  val_acc=%.3f\n", model.ExclusiveAcc)
	}
	if len(model.FitWeights) > 0 {
		fmt.Printf("  fit head:        val_acc=%.3f\n", model.FitAcc)
	}
	return nil
}

func intentOnlineTrainingEnabled() bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv("OC_INTENT_ONLINE_TRAIN")))
	switch v {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func ensureOnlineIntentModel() *IntentLogRegModel {
	if m := loadIntentLogRegModel(); m != nil {
		return m
	}
	m := &IntentLogRegModel{
		Version:   2,
		Feature:   "hash",
		Dim:       4096,
		Weights:   make([]float64, 4096),
		Bias:      0,
		Samples:   0,
		TrainAcc:  0.5,
		Source:    "online",
		UpdatedAt: time.Now(),
	}
	intentModelState.Lock()
	intentModelState.model = m
	intentModelState.loaded = true
	intentModelState.Unlock()
	return m
}

func onlineTrainIntentFromTurn(prompt string, executed bool) error {
	if !intentOnlineTrainingEnabled() {
		return nil
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return nil
	}

	if loadIntentLogRegModel() == nil {
		_ = ensureOnlineIntentModel()
	}

	intentModelState.Lock()
	model := intentModelState.model
	if model == nil {
		intentModelState.Unlock()
		return fmt.Errorf("intent model unavailable")
	}
	x := intentFeatureVector(prompt, model)
	if len(x) != model.Dim || len(model.Weights) != model.Dim {
		intentModelState.Unlock()
		return fmt.Errorf("intent model/features dimension mismatch")
	}

	y := 0.0
	if executed {
		y = 1.0
	}

	z := model.Bias
	for i := 0; i < model.Dim; i++ {
		z += model.Weights[i] * x[i]
	}
	p := sigmoid(z)
	errTerm := p - y

	if math.Abs(errTerm) < 0.06 {
		model.Samples++
		model.UpdatedAt = time.Now()
		snap := cloneIntentModel(model)
		intentModelState.Unlock()
		if snap.Samples%50 == 0 {
			_ = writeIntentLogRegModelFile(snap)
		}
		return nil
	}

	lrBase := 0.03
	lr := lrBase / math.Sqrt(1.0+float64(model.Samples)/300.0)
	l2 := 1e-5

	for i, xv := range x {
		model.Weights[i] -= lr * (errTerm*xv + l2*model.Weights[i])
	}
	model.Bias -= lr * errTerm
	model.Samples++
	model.UpdatedAt = time.Now()
	if model.Source == "" {
		model.Source = "online"
	}
	pred := 0.0
	if p >= 0.5 {
		pred = 1
	}
	correct := 0.0
	if pred == y {
		correct = 1
	}
	model.TrainAcc = model.TrainAcc*0.995 + correct*0.005

	snap := cloneIntentModel(model)
	intentModelState.Unlock()

	if snap.Samples%25 == 0 {
		if err := writeIntentLogRegModelFile(snap); err != nil {
			return err
		}
		_ = writeGlobalIntentLogRegModelFile(snap)
	}
	if traceJIT {
		traceLog("[jit] online intent train: y=%.0f p=%.2f err=%.2f samples=%d", y, p, errTerm, snap.Samples)
	}
	return nil
}

func cloneIntentModel(m *IntentLogRegModel) *IntentLogRegModel {
	if m == nil {
		return nil
	}
	cp := *m
	cp.Weights = append([]float64(nil), m.Weights...)
	if m.ExclusiveWeights != nil {
		cp.ExclusiveWeights = append([]float64(nil), m.ExclusiveWeights...)
	}
	if m.FitWeights != nil {
		cp.FitWeights = append([]float64(nil), m.FitWeights...)
	}
	return &cp
}

func intentFeatureVector(prompt string, model *IntentLogRegModel) []float64 {
	if model == nil {
		return nil
	}
	return hashPromptFeatures(prompt, model.Dim)
}

// hashPromptFeatures builds a feature vector using hashed token features
// plus hand-crafted structural features.
func hashPromptFeatures(prompt string, dim int) []float64 {
	if dim <= 0 {
		return nil
	}
	v := make([]float64, dim)
	lower := strings.ToLower(prompt)
	tokens := strings.Fields(lower)
	count := 0.0

	for _, t := range tokens {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		idx := int(hashStr(t) % uint64(dim))
		v[idx] += 1.0
		count++
	}

	for i := 0; i+1 < len(tokens); i++ {
		bg := tokens[i] + "|" + tokens[i+1]
		idx := int(hashStr(bg) % uint64(dim))
		v[idx] += 0.7
		count += 0.7
	}

	padded := "^" + lower + "$"
	for i := 0; i+3 <= len(padded); i++ {
		tri := padded[i : i+3]
		idx := int(hashStr("c3:"+tri) % uint64(dim))
		v[idx] += 0.3
		count += 0.3
	}

	structBase := dim - 32
	if structBase < 0 {
		structBase = 0
	}

	wc := len(tokens)
	switch {
	case wc <= 2:
		v[structBase] = 1.0
	case wc <= 5:
		v[structBase+1] = 1.0
	case wc <= 10:
		v[structBase+2] = 1.0
	default:
		v[structBase+3] = 1.0
	}

	if strings.HasSuffix(strings.TrimSpace(prompt), "?") {
		v[structBase+4] = 1.0
	}

	if isImperativePrompt(lower) {
		v[structBase+5] = 1.5
	}

	cmdPrefixes := []string{"go ", "cargo ", "npm ", "yarn ", "pip ", "python ", "make ", "docker ", "git "}
	for _, p := range cmdPrefixes {
		if strings.HasPrefix(lower, p) {
			v[structBase+6] = 1.5
			break
		}
	}

	questionWords := []string{"what", "why", "how", "where", "when", "which", "who", "does", "is", "are", "can", "should", "would", "could"}
	if len(tokens) > 0 {
		for _, qw := range questionWords {
			if tokens[0] == qw {
				v[structBase+7] = 1.0
				break
			}
		}
	}

	connectives := []string{" and ", " then ", " also ", " plus ", " after "}
	for _, c := range connectives {
		if strings.Contains(lower, c) {
			v[structBase+8] = 1.0
			break
		}
	}

	rerunWords := []string{"again", "rerun", "retry", "redo", "repeat"}
	for _, rw := range rerunWords {
		if strings.Contains(lower, rw) {
			v[structBase+9] = 1.0
			break
		}
	}

	allSimple := true
	for _, r := range prompt {
		if !unicode.IsLower(r) && !unicode.IsDigit(r) && r != ' ' && r != '/' && r != '.' && r != '-' && r != '_' {
			allSimple = false
			break
		}
	}
	if allSimple && wc <= 5 {
		v[structBase+10] = 1.0
	}

	domainKW := []string{"test", "build", "lint", "check", "vet", "compile", "format", "fmt"}
	for _, kw := range domainKW {
		if strings.Contains(lower, kw) {
			v[structBase+11] = 1.0
			break
		}
	}

	if count > 0 {
		inv := 1.0 / math.Sqrt(count)
		for i := 0; i < structBase; i++ {
			v[i] *= inv
		}
	}

	return v
}

func hashStr(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}
