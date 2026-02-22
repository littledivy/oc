package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/html"
)

var toolDefinitions = `[
  {
    "name": "bash",
    "description": "Run terminal commands (build/test/git/runtime). Do not use bash for file search/read/edit operations when dedicated tools exist. Commands execute in the current working directory; do not guess or probe alternate roots (like /repo or /root) unless the user explicitly asked.",
    "input_schema": {
      "type": "object",
      "properties": {
        "command": {"type": "string", "description": "The shell command to execute"}
      },
      "required": ["command"]
    }
  },
  {
    "name": "grep",
    "description": "Search text in files using a bounded ripgrep wrapper. Prefer this over running rg/grep via bash for code search. By default ignores .git and node_modules unless explicitly targeting those paths.",
    "input_schema": {
      "type": "object",
      "properties": {
        "pattern": {"type": "string", "description": "Pattern to search for (literal text or regex)"},
        "path": {"type": "string", "description": "Directory or file path to search (default: .)"},
        "include": {"type": "string", "description": "Optional glob include filter, e.g. *.go or **/*.ts"},
        "max_results": {"type": "string", "description": "Optional maximum number of matches to return (default 100, max 200)"}
      },
      "required": ["pattern"]
    }
  },
  {
    "name": "glob",
    "description": "Find files by glob pattern recursively. Prefer this over shell glob expansion for repository discovery.",
    "input_schema": {
      "type": "object",
      "properties": {
        "pattern": {"type": "string", "description": "Glob pattern, e.g. **/*.go or src/**/*.ts"},
        "path": {"type": "string", "description": "Directory root to search from (default: .)"}
      },
      "required": ["pattern"]
    }
  },
  {
    "name": "webfetch",
    "description": "Fetch content from an HTTP/HTTPS URL and return readable text.",
    "input_schema": {
      "type": "object",
      "properties": {
        "url": {"type": "string", "description": "URL to fetch"},
        "max_len": {"type": "string", "description": "Optional max output characters (default 20000, max 50000)"}
      },
      "required": ["url"]
    }
  },
  {
    "name": "find_symbol",
    "description": "Find symbol definitions/usages using backend auto|lsp|ctags|tree_sitter. Supports bounded results or all matches. By default ignores .git and node_modules unless explicitly targeting those paths.",
    "input_schema": {
      "type": "object",
      "properties": {
        "symbol": {"type": "string", "description": "Symbol name to find"},
        "path": {"type": "string", "description": "Directory/file to search (default: .)"},
        "backend": {"type": "string", "description": "Search backend: auto, lsp, ctags, tree_sitter"},
        "include": {"type": "string", "description": "Optional glob include filter"},
        "all": {"type": "string", "description": "If true, return all matches (bounded by safety cap)"},
        "max_results": {"type": "string", "description": "Maximum matches to return when all=false (default 20, max 500)"}
      },
      "required": ["symbol"]
    }
  },
  {
    "name": "read_files",
    "description": "Read multiple files in one call.",
    "input_schema": {
      "type": "object",
      "properties": {
        "paths": {"type": "array", "items": {"type": "string"}, "description": "Paths to files to read"}
      },
      "required": ["paths"]
    }
  },
  {
    "name": "read_file",
    "description": "Read the contents of a file.",
    "input_schema": {
      "type": "object",
      "properties": {
        "path": {"type": "string", "description": "Path to the file to read"},
        "start_line": {"type": "string", "description": "Optional 1-based start line for partial read"},
        "end_line": {"type": "string", "description": "Optional 1-based end line for partial read"},
        "max_chars": {"type": "string", "description": "Optional max characters to return for this read"}
      },
      "required": ["path"]
    }
  },
  {
    "name": "write_file",
    "description": "Write content to a file, creating it if necessary.",
    "input_schema": {
      "type": "object",
      "properties": {
        "path": {"type": "string", "description": "Path to the file to write"},
        "content": {"type": "string", "description": "Content to write to the file"}
      },
      "required": ["path", "content"]
    }
  },
  {
    "name": "write_files",
    "description": "Write multiple files in one call.",
    "input_schema": {
      "type": "object",
      "properties": {
        "files": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "path": {"type": "string"},
              "content": {"type": "string"}
            },
            "required": ["path", "content"]
          },
          "description": "List of file writes"
        }
      },
      "required": ["files"]
    }
  },
  {
    "name": "edit_file",
    "description": "Edit a file by replacing an exact string match. The old_string must match exactly (including whitespace/indentation). Returns an error with context if no match is found, without requiring an API round-trip.",
    "input_schema": {
      "type": "object",
      "properties": {
        "path": {"type": "string", "description": "Path to the file to edit"},
        "old_string": {"type": "string", "description": "The exact string to find and replace. Must be unique in the file."},
        "new_string": {"type": "string", "description": "The replacement string"}
      },
      "required": ["path", "old_string", "new_string"]
    }
  },
  {
    "name": "list_files",
    "description": "List files and directories in a given path.",
    "input_schema": {
      "type": "object",
      "properties": {
        "path": {"type": "string", "description": "Directory path to list"}
      },
      "required": ["path"]
    }
  },
  {
    "name": "todowrite",
    "description": "Replace the current todo list atomically. Input may be either {\"todos\":[...]} or a raw array.",
    "input_schema": {
      "type": "object",
      "properties": {
        "todos": {
          "type": "array",
          "items": {
            "type": "object",
            "properties": {
              "id": {"type": "string"},
              "content": {"type": "string"},
              "status": {"type": "string", "description": "pending | in_progress | completed | cancelled"},
              "priority": {"type": "string", "description": "high | medium | low"}
            },
            "required": ["id", "content", "status", "priority"]
          }
        }
      },
      "required": ["todos"]
    }
  },
  {
    "name": "regex_edit",
    "description": "Apply a regex find-and-replace across multiple files in one call. Use this instead of repeated read_file+edit_file when making the same mechanical change across many files (e.g., adding attributes, renaming symbols, inserting lines before/after a pattern). Supports Go regexp syntax with capture groups ($1, $2, etc.) in the replacement. Dry-run by default for safety — set dry_run to false to apply.",
    "input_schema": {
      "type": "object",
      "properties": {
        "pattern": {"type": "string", "description": "Go-syntax regex pattern to match"},
        "replacement": {"type": "string", "description": "Replacement string. Use $1, $2 for capture groups."},
        "glob": {"type": "string", "description": "File glob pattern, e.g. **/*.rs or src/**/*.go"},
        "dry_run": {"type": "string", "description": "If 'true' (default), show what would change without modifying files. Set to 'false' to apply."}
      },
      "required": ["pattern", "replacement", "glob"]
    }
  },
  {
    "name": "code",
    "description": "Execute TypeScript code and save it as a reusable skill. The code runs via Deno with oc.* bindings: oc.read(path), oc.write(path, content), oc.edit(path, old, new), oc.glob(pattern), oc.grep(pattern, path?), oc.bash(cmd), oc.list(path?), oc.ask(prompt). Use console.log() for output.",
    "input_schema": {
      "type": "object",
      "properties": {
        "code": {"type": "string", "description": "TypeScript code to execute"},
        "skill_name": {"type": "string", "description": "Short unique name for this skill, e.g. find_todos, run_tests, add_endpoint"},
        "skill_description": {"type": "string", "description": "What this code does, e.g. 'Find all TODO comments in Go files'"}
      },
      "required": ["code", "skill_name", "skill_description"]
    }
  },
  {
    "name": "run_skill",
    "description": "Execute a saved code skill by name.",
    "input_schema": {
      "type": "object",
      "properties": {
        "name": {"type": "string", "description": "Name of the skill to run"}
      },
      "required": ["name"]
    }
  }
]`

type ToolResult struct {
	Content string
	IsError bool
}

// buildRunSkillDescription returns a dynamic description for run_skill
// that lists all available skills so the LLM sees them upfront.
func buildRunSkillDescription() string {
	base := "Execute a saved code skill by name."
	if jitEngine == nil || jitEngine.CodeSkills == nil || len(jitEngine.CodeSkills.Skills) == 0 {
		return base + " No skills available yet."
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteString(" Available skills: ")
	first := true
	for _, s := range jitEngine.CodeSkills.Skills {
		if s == nil || s.Name == "" {
			continue
		}
		if !first {
			b.WriteString(", ")
		}
		first = false
		b.WriteString(s.Name)
		if s.Description != "" {
			b.WriteString(" (")
			desc := s.Description
			if len(desc) > 50 {
				desc = desc[:47] + "..."
			}
			b.WriteString(desc)
			b.WriteString(")")
		}
	}
	b.WriteString(".")
	return b.String()
}

// codeToolOnly filters out tools that the code tool subsumes.
// save_skill is hidden because run_skill already surfaces available skills.
var codeToolOnly = map[string]bool{
	"save_skill": true,
}

// buildToolDefinitions returns tool definitions JSON with dynamic skill list
// embedded in the run_skill description so the LLM sees available skills upfront.
// Tools in codeToolOnly are hidden from the LLM but still executable via dispatch.
func buildToolDefinitions() string {
	desc := buildRunSkillDescription()

	var tools []map[string]any
	if err := json.Unmarshal([]byte(toolDefinitions), &tools); err != nil {
		return toolDefinitions
	}

	filtered := tools[:0]
	for i := range tools {
		name, _ := tools[i]["name"].(string)
		if codeToolOnly[name] {
			continue
		}
		if name == "run_skill" {
			tools[i]["description"] = desc
		}
		filtered = append(filtered, tools[i])
	}

	out, err := json.Marshal(filtered)
	if err != nil {
		return toolDefinitions
	}
	return string(out)
}

// lastBashOutput stores the full output of the last bash command for Ctrl+O expansion.
var lastBashOutput string

// lastEditedFile stores the absolute path of the last file written, for Ctrl+E.
var lastEditedFile string

// liveOutputBuffer is a thread-safe io.Writer that accumulates command output
// for live display during execution.
type liveOutputBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *liveOutputBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// Snapshot returns the last maxLines lines of accumulated output.
func (l *liveOutputBuffer) Snapshot(maxLines int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.buf.String()
	if maxLines <= 0 || s == "" {
		return s
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func (l *liveOutputBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

var activeCmdOutput *liveOutputBuffer
var activeCmdMu sync.Mutex
var showLiveOutput atomic.Bool

func setActiveCmdOutput(lb *liveOutputBuffer) {
	activeCmdMu.Lock()
	activeCmdOutput = lb
	activeCmdMu.Unlock()
}

func clearActiveCmdOutput() {
	activeCmdMu.Lock()
	activeCmdOutput = nil
	activeCmdMu.Unlock()
	showLiveOutput.Store(false)
}

func getActiveCmdSnapshot(maxLines int) string {
	activeCmdMu.Lock()
	lb := activeCmdOutput
	activeCmdMu.Unlock()
	if lb == nil {
		return ""
	}
	return lb.Snapshot(maxLines)
}

func executeTool(name string, inputJSON json.RawMessage) ToolResult {
	var input map[string]string
	json.Unmarshal(inputJSON, &input)

	if currentMode == ModePlan {
		switch name {
		case "write_file", "write_files", "edit_file", "regex_edit":
			return ToolResult{
				Content: "Plan mode is active: write/edit tools are disabled. Switch to build mode with /build or /mode build to implement changes.",
				IsError: true,
			}
		case "bash":
			cmd := input["command"]
			if !isReadOnlyBash(cmd) {
				return ToolResult{
					Content: "Plan mode is active: non-read-only bash is disabled. Use read-only commands or switch to build mode.",
					IsError: true,
				}
			}
		}
	}

	switch name {
	case "bash":
		return execBashClean(input["command"])
	case "grep":
		return execGrep(input["pattern"], input["path"], input["include"], input["max_results"])
	case "glob":
		return execGlob(input["pattern"], input["path"])
	case "webfetch":
		return execWebfetch(input["url"], input["max_len"])
	case "find_symbol":
		return execFindSymbol(input["symbol"], input["path"], input["backend"], input["include"], input["all"], input["max_results"])
	case "read_files":
		return execReadFiles(parseReadFilesArgs(inputJSON))
	case "read_file":
		a := parseReadFileArgs(inputJSON)
		return execReadFileWithOptions(a.Path, a.StartLine, a.EndLine, a.MaxChars)
	case "write_files":
		return execWriteFiles(parseWriteFilesArgs(inputJSON))
	case "write_file":
		return execWriteFile(input["path"], input["content"])
	case "edit_file":
		return execEditFile(input["path"], input["old_string"], input["new_string"])
	case "list_files":
		return execListFiles(input["path"])
	case "todowrite":
		return execTodoWrite(inputJSON)
	case "regex_edit":
		return execRegexEdit(input["pattern"], input["replacement"], input["glob"], input["dry_run"])
	case "code":
		result := execCode(input["code"])
		if !result.IsError && input["skill_name"] != "" {
			execSaveSkill(input["skill_name"], input["skill_description"], "", input["code"])
		}
		return result
	case "run_skill":
		return execRunSkill(input["name"])
	default:
		return ToolResult{Content: fmt.Sprintf("Unknown tool: %s", name), IsError: true}
	}
}

type symbolMatch struct {
	Path string
	Line string
	Text string
}

type writeFileBatchItem struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type readFileArgs struct {
	Path      string
	StartLine int
	EndLine   int
	MaxChars  int
}

func parseReadFilesArgs(input json.RawMessage) []string {
	var a struct {
		Paths []string `json:"paths"`
	}
	_ = json.Unmarshal(input, &a)
	return a.Paths
}

func parseReadFileArgs(input json.RawMessage) readFileArgs {
	var raw map[string]any
	_ = json.Unmarshal(input, &raw)
	path, _ := raw["path"].(string)
	return readFileArgs{
		Path:      strings.TrimSpace(path),
		StartLine: parseAnyPositiveInt(raw["start_line"]),
		EndLine:   parseAnyPositiveInt(raw["end_line"]),
		MaxChars:  parseAnyPositiveInt(raw["max_chars"]),
	}
}

func parseAnyPositiveInt(v any) int {
	switch n := v.(type) {
	case float64:
		if int(n) > 0 {
			return int(n)
		}
	case string:
		if p, ok := parsePositiveInt(n); ok {
			return p
		}
	}
	return 0
}

func parseWriteFilesArgs(input json.RawMessage) []writeFileBatchItem {
	var a struct {
		Files []writeFileBatchItem `json:"files"`
	}
	_ = json.Unmarshal(input, &a)
	return a.Files
}

func execReadFiles(paths []string) ToolResult {
	if len(paths) == 0 {
		return ToolResult{Content: "Error: paths is required", IsError: true}
	}
	maxBytes := envInt("OC_MAX_TOOL_OUTPUT_BYTES", 40000)
	var b strings.Builder
	written := 0
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if written > 0 {
			b.WriteString("\n\n")
		}

		res := execReadFile(p)
		if b.Len()+len(res.Content) > maxBytes && written > 0 {
			ev := SliceFile(p, nil)
			if ev != nil && len(ev.Slices) > 0 {
				var sliced strings.Builder
				for _, s := range ev.Slices {
					sliced.WriteString(s.Content)
					sliced.WriteString("\n")
				}
				fmt.Fprintf(&b, "=== %s (outline — use read_file for full content) ===\n%s", p, sliced.String())
			} else {
				content := res.Content
				remaining := maxBytes - b.Len() - 200
				if remaining > 0 && len(content) > remaining {
					content = content[:remaining] + fmt.Sprintf("\n... (truncated %d/%d chars — use read_file for full content)", remaining, len(res.Content))
				}
				fmt.Fprintf(&b, "=== %s ===\n%s", p, content)
			}
		} else {
			fmt.Fprintf(&b, "=== %s ===\n%s", p, res.Content)
		}
		written++
	}
	if written == 0 {
		return ToolResult{Content: "Error: no valid paths provided", IsError: true}
	}
	return ToolResult{Content: b.String(), IsError: false}
}

func execWriteFiles(files []writeFileBatchItem) ToolResult {
	if len(files) == 0 {
		return ToolResult{Content: "Error: files is required", IsError: true}
	}
	var (
		b      strings.Builder
		failed int
		done   int
	)
	for _, f := range files {
		p := strings.TrimSpace(f.Path)
		if p == "" {
			failed++
			continue
		}
		res := execWriteFile(p, f.Content)
		done++
		if res.IsError {
			failed++
			fmt.Fprintf(&b, "FAILED %s: %s\n", p, res.Content)
		} else {
			fmt.Fprintf(&b, "WROTE %s\n", p)
		}
	}
	if done == 0 {
		return ToolResult{Content: "Error: no valid files provided", IsError: true}
	}
	out := strings.TrimSpace(b.String())
	if failed > 0 {
		out += fmt.Sprintf("\n\n<tool_metadata>\npartial_failure=true\nfailed=%d\ntotal=%d\n</tool_metadata>", failed, len(files))
	}
	return ToolResult{Content: out, IsError: failed == done}
}

func defaultExcludeHeavyDirs(path string) bool {
	p := filepath.ToSlash(strings.ToLower(strings.TrimSpace(path)))
	if p == "" {
		return true
	}
	if strings.Contains(p, "/node_modules/") || strings.HasSuffix(p, "/node_modules") || p == "node_modules" {
		return false
	}
	if strings.Contains(p, "/.git/") || strings.HasSuffix(p, "/.git") || p == ".git" {
		return false
	}
	return true
}

func shouldFilterPathLine(lineOrPath, rootPath string) bool {
	if !defaultExcludeHeavyDirs(rootPath) {
		return false
	}
	p := filepath.ToSlash(strings.ToLower(lineOrPath))
	return strings.Contains(p, "/node_modules/") || strings.Contains(p, "/.git/")
}

func formatSymbolMatches(symbol, backend, detail string, matches []symbolMatch, maxResults int, all bool) ToolResult {
	orig := len(matches)
	truncated := false
	if !all && len(matches) > maxResults {
		matches = matches[:maxResults]
		truncated = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Symbol %q via %s", symbol, backend)
	if detail != "" {
		fmt.Fprintf(&b, " (%s)", detail)
	}
	b.WriteByte('\n')
	for _, m := range matches {
		text := strings.TrimSpace(m.Text)
		if len(text) > 500 {
			text = text[:497] + "..."
		}
		fmt.Fprintf(&b, "%s:%s: %s\n", m.Path, m.Line, text)
	}
	if truncated {
		fmt.Fprintf(&b, "\n... truncated to %d/%d matches", len(matches), orig)
	}
	b.WriteString(fmt.Sprintf("\n\n<tool_metadata>\nbackend=%s\ntruncated=%t\ntotal_matches=%d\nshown_matches=%d\n</tool_metadata>",
		backend, truncated, orig, len(matches)))
	return ToolResult{Content: truncateToolOutput("find_symbol", strings.TrimRight(b.String(), "\n")), IsError: false}
}

func parseBoolLike(raw string) bool {
	s := strings.ToLower(strings.TrimSpace(raw))
	return s == "1" || s == "true" || s == "yes" || s == "y" || s == "on"
}

func regexpQuote(s string) string {
	repl := strings.NewReplacer(
		`\\`, `\\\\`,
		`.`, `\.`,
		`+`, `\+`,
		`*`, `\*`,
		`?`, `\?`,
		`(`, `\(`,
		`)`, `\)`,
		`[`, `\[`,
		`]`, `\]`,
		`{`, `\{`,
		`}`, `\}`,
		`^`, `\^`,
		`$`, `\$`,
		`|`, `\|`,
	)
	return repl.Replace(s)
}

type grepMatch struct {
	Path string
	Line string
	Text string
}

func execGlob(pattern, path string) ToolResult {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ToolResult{Content: "Error: pattern is required", IsError: true}
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}

	if out, ok := execGlobWithRG(pattern, path); ok {
		return out
	}
	return execGlobWithWalk(pattern, path)
}

func execGlobWithRG(pattern, path string) (ToolResult, bool) {
	if _, err := exec.LookPath("rg"); err != nil {
		return ToolResult{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	args := []string{"--files", "--hidden", "--glob", pattern}
	if defaultExcludeHeavyDirs(path) {
		args = append(args, "--glob", "!**/node_modules/**", "--glob", "!**/.git/**")
	}
	args = append(args, path)
	cmd := exec.CommandContext(ctx, "rg", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return ToolResult{Content: "No matches found.", IsError: false}, true
		}
		return ToolResult{}, false
	}

	lines := splitNonEmptyLines(string(out))
	if len(lines) == 0 {
		return ToolResult{Content: "No matches found.", IsError: false}, true
	}
	sort.Strings(lines)
	return formatGlobResults(lines), true
}

func execGlobWithWalk(pattern, root string) ToolResult {
	re, err := compileGlobRegex(pattern)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: invalid glob pattern: %v", err), IsError: true}
	}

	matches := make([]string, 0, 128)
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && defaultExcludeHeavyDirs(root) {
			name := strings.ToLower(d.Name())
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if re.MatchString(rel) {
			matches = append(matches, filepath.ToSlash(path))
		}
		return nil
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error scanning files: %v", err), IsError: true}
	}
	if len(matches) == 0 {
		return ToolResult{Content: "No matches found.", IsError: false}
	}
	sort.Strings(matches)
	return formatGlobResults(matches)
}

func compileGlobRegex(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	p := filepath.ToSlash(strings.TrimSpace(pattern))
	for i := 0; i < len(p); i++ {
		ch := p[i]
		switch ch {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				b.WriteString(".*")
				i++
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(ch)
		default:
			b.WriteByte(ch)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func splitNonEmptyLines(s string) []string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			out = append(out, filepath.ToSlash(ln))
		}
	}
	return out
}

func formatGlobResults(matches []string) ToolResult {
	orig := len(matches)
	maxShown := 2000
	truncated := false
	if len(matches) > maxShown {
		matches = matches[:maxShown]
		truncated = true
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n\n<tool_metadata>\ntruncated=true\ntool=glob\ntotal_matches=%d\nshown_matches=%d\n</tool_metadata>", orig, len(matches))
	}
	return ToolResult{Content: truncateToolOutput("glob", out), IsError: false}
}

func execGrep(pattern, path, include, maxResultsRaw string) ToolResult {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return ToolResult{Content: "Error: pattern is required", IsError: true}
	}
	path = strings.TrimSpace(path)
	if path == "" {
		path = "."
	}
	maxResults := 100
	if n, ok := parsePositiveInt(maxResultsRaw); ok {
		maxResults = n
	}
	if maxResults > 200 {
		maxResults = 200
	}
	if maxResults < 1 {
		maxResults = 1
	}

	if _, err := exec.LookPath("rg"); err != nil {
		return ToolResult{Content: "Error: rg (ripgrep) is not installed", IsError: true}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	args := []string{
		"-nH",
		"--hidden",
		"--no-messages",
		"--color", "never",
		"--field-match-separator=|",
		"--", pattern,
	}
	if g := strings.TrimSpace(include); g != "" {
		args = append(args, "--glob", g)
	}
	if defaultExcludeHeavyDirs(path) {
		args = append(args, "--glob", "!**/node_modules/**", "--glob", "!**/.git/**")
	}
	args = append(args, path)

	cmd := exec.CommandContext(ctx, "rg", args...)
	out, err := cmd.Output()
	exitCode := 0
	hadPartialError := false
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		if exitCode == 1 {
			return ToolResult{Content: "No matches found.", IsError: false}
		}
		if exitCode == 2 && strings.TrimSpace(string(out)) != "" {
			hadPartialError = true
		} else if exitCode == 2 {
			return ToolResult{Content: "Search encountered errors (rg exit 2) and produced no results. Narrow path/include/pattern.", IsError: true}
		} else {
			if ctx.Err() == context.DeadlineExceeded {
				return ToolResult{Content: "Search timed out after 8s. Narrow path/include/pattern.", IsError: true}
			}
			return ToolResult{Content: fmt.Sprintf("Error running rg: %v", err), IsError: true}
		}
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	matches := make([]grepMatch, 0, len(lines))
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, "|", 3)
		if len(parts) < 3 {
			continue
		}
		txt := parts[2]
		if len(txt) > 600 {
			txt = txt[:597] + "..."
		}
		matches = append(matches, grepMatch{Path: parts[0], Line: parts[1], Text: txt})
	}
	if len(matches) == 0 {
		return ToolResult{Content: "No matches found.", IsError: false}
	}

	sort.SliceStable(matches, func(i, j int) bool {
		iInfo, iErr := os.Stat(matches[i].Path)
		jInfo, jErr := os.Stat(matches[j].Path)
		if iErr != nil || jErr != nil {
			return matches[i].Path < matches[j].Path
		}
		return iInfo.ModTime().After(jInfo.ModTime())
	})

	orig := len(matches)
	truncated := false
	if len(matches) > maxResults {
		matches = matches[:maxResults]
		truncated = true
	}

	var b strings.Builder
	for _, m := range matches {
		b.WriteString(m.Path)
		b.WriteByte(':')
		b.WriteString(m.Line)
		b.WriteString(": ")
		b.WriteString(m.Text)
		b.WriteByte('\n')
	}
	if truncated {
		fmt.Fprintf(&b, "\n... truncated to %d/%d matches", len(matches), orig)
	}
	outText := strings.TrimRight(b.String(), "\n")
	if truncated {
		outText += fmt.Sprintf("\n\n<tool_metadata>\ntruncated=true\ntool=grep\ntotal_matches=%d\nshown_matches=%d\n</tool_metadata>", orig, len(matches))
	}
	if hadPartialError {
		outText += "\n\n<tool_metadata>\npartial_error=true\nnote=rg exited 2 but returned partial results\n</tool_metadata>"
	}
	outText = truncateToolOutput("grep", outText)
	return ToolResult{Content: outText, IsError: false}
}

func parsePositiveInt(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func truncateToolOutput(tool, out string) string {
	maxBytes := envInt("OC_MAX_TOOL_OUTPUT_BYTES", 40000)
	if maxBytes <= 0 || len(out) <= maxBytes {
		return out
	}
	originalBytes := len(out)
	trimmed := strings.TrimRight(out[:maxBytes], "\n")

	// Save full output to disk so the LLM can access it later.
	dir := filepath.Join(".oc", "tool-output")
	_ = os.MkdirAll(dir, 0o755)
	fname := fmt.Sprintf("%d-%s.txt", time.Now().Unix(), tool)
	fpath := filepath.Join(dir, fname)
	savedMsg := ""
	if err := os.WriteFile(fpath, []byte(out), 0o644); err == nil {
		savedMsg = fmt.Sprintf("\n[Full output saved to %s]", fpath)
	}

	hint := fmt.Sprintf("\n\n[Output truncated: %d bytes total, showing first %d]%s\n[Use read_file with start_line/end_line to access specific sections]",
		originalBytes, len(trimmed), savedMsg)
	meta := fmt.Sprintf("\n\n<tool_metadata>\ntruncated=true\ntool=%s\noriginal_bytes=%d\nreturned_bytes=%d\n</tool_metadata>", tool, originalBytes, len(trimmed))
	return trimmed + hint + meta
}

// execBashClean runs a command and captures output without terminal display interference.
// Full output is available via Ctrl+O, with a simple summary shown after completion.
// bashFileIOPattern matches bash commands that read/write files directly.
// These should go through the code tool instead.
var bashFileIOCommands = []string{"cat ", "head ", "tail ", "less ", "more ", "tee ", "cp ", "mv ", "ls ", "ls\n", "find "}

func isBashFileIO(command string) bool {
	cmd := strings.TrimSpace(command)
	for _, prefix := range bashFileIOCommands {
		if strings.HasPrefix(cmd, prefix) || strings.HasPrefix(cmd, "sudo "+prefix) {
			return true
		}
	}
	// Also catch redirections used for writing: echo/printf > file
	if (strings.HasPrefix(cmd, "echo ") || strings.HasPrefix(cmd, "printf ")) &&
		(strings.Contains(cmd, " > ") || strings.Contains(cmd, " >> ")) {
		return true
	}
	return false
}

func execBashClean(command string) ToolResult {
	if isBashFileIO(command) {
		return ToolResult{
			Content: "Use the code tool for file operations. bash is for build/test/git/runtime commands. Example: code tool with oc.read(), oc.write(), oc.list().",
			IsError: true,
		}
	}
	timeoutSec := 45
	if raw := strings.TrimSpace(os.Getenv("OC_BASH_TIMEOUT_SEC")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			timeoutSec = n
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutSec)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout pipe

	if err := cmd.Start(); err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: %v", err), IsError: true}
	}

	lb := &liveOutputBuffer{}
	setActiveCmdOutput(lb)
	defer clearActiveCmdOutput()

	scanner := bufio.NewScanner(io.TeeReader(pipe, lb))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
	}

	cmdErr := cmd.Wait()

	result := truncateToolOutput("bash", lb.String())

	lastBashOutput = result

	if cmdErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			result = fmt.Sprintf("Command timed out after %ds\n%s", timeoutSec, result)
			return ToolResult{Content: result, IsError: true}
		}
		if exitErr, ok := cmdErr.(*exec.ExitError); ok {
			code := exitErr.ExitCode()
			result = fmt.Sprintf("Exit code: %d\n%s", code, result)
			if isBenignExitCode(command, code) {
				return ToolResult{Content: result, IsError: false}
			}
		} else {
			result = fmt.Sprintf("Error: %v\n%s", cmdErr, result)
		}
		return ToolResult{Content: result, IsError: true}
	}

	return ToolResult{Content: result, IsError: false}
}

// isBenignExitCode returns true for tools where a non-zero exit code is normal
// and should not be treated as an error (e.g. grep returns 1 for no matches,
// diff returns 1 when files differ).
func isBenignExitCode(command string, exitCode int) bool {
	if exitCode != 1 {
		return false
	}
	cmd := strings.TrimSpace(command)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	binary := filepath.Base(fields[0])
	switch binary {
	case "grep", "egrep", "fgrep", "rg", "ag", "ack", "diff":
		return true
	}
	return false
}

func execReadFile(path string) ToolResult {
	return execReadFileWithOptions(path, 0, 0, 0)
}

func execReadFileWithOptions(path string, startLine, endLine, maxChars int) ToolResult {
	path = strings.TrimSpace(path)
	if path == "" {
		return ToolResult{Content: "Error: path is required", IsError: true}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error reading file: %v", err), IsError: true}
	}
	content := string(data)
	if startLine > 0 || endLine > 0 {
		content = readFileLineRange(content, startLine, endLine)
	}
	if maxChars > 0 && len(content) > maxChars {
		content = strings.TrimRight(content[:maxChars], "\n")
		content += fmt.Sprintf("\n\n<tool_metadata>\ntruncated=true\ntool=read_file\nlimit=max_chars\nmax_chars=%d\n</tool_metadata>", maxChars)
	}
	return ToolResult{Content: truncateToolOutput("read_file", content), IsError: false}
}

func readFileLineRange(content string, startLine, endLine int) string {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return content
	}
	if startLine <= 0 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > len(lines) {
		startLine = len(lines)
	}
	if endLine < startLine {
		endLine = startLine
	}
	out := strings.Join(lines[startLine-1:endLine], "\n")
	out += fmt.Sprintf("\n\n<tool_metadata>\nline_range=%d-%d\ntotal_lines=%d\n</tool_metadata>", startLine, endLine, len(lines))
	return out
}

func execWriteFile(path, content string) ToolResult {
	dir := filepath.Dir(path)
	if dir != "." {
		os.MkdirAll(dir, 0o755)
	}

	absPath, _ := filepath.Abs(path)

	oldData, readErr := os.ReadFile(path)

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}
	}

	lastEditedFile = absPath

	var display strings.Builder
	if readErr == nil {
		display.WriteString(dim("wrote " + path + "  "))
		display.WriteString("\033[2;7m ctrl+e to edit \033[0m")
		display.WriteString("\n")
		display.WriteString(renderDiff(string(oldData), content, path))
	} else {
		display.WriteString(dim("created " + path + "  "))
		display.WriteString("\033[2;7m ctrl+e to edit \033[0m")
		display.WriteString("\n")
		display.WriteString(renderNewFile(content, path))
	}
	fmt.Println(display.String())

	return ToolResult{Content: fmt.Sprintf("Successfully wrote to %s", path), IsError: false}
}

// execEditFile performs a string replacement in a file with local pre-validation.
// If old_string is not found, it returns a helpful error with the nearest match
// instead of burning an API round-trip.
func execEditFile(path, oldString, newString string) ToolResult {
	if oldString == newString {
		return ToolResult{Content: "Error: old_string and new_string are identical", IsError: true}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error reading file: %v", err), IsError: true}
	}
	content := string(data)

	count := strings.Count(content, oldString)
	if count == 0 {
		hint := findNearestMatch(content, oldString)
		msg := fmt.Sprintf("Error: old_string not found in %s.", path)
		if hint != "" {
			msg += fmt.Sprintf("\n\nDid you mean:\n%s", hint)
		}
		return ToolResult{Content: msg, IsError: true}
	}
	if count > 1 {
		return ToolResult{
			Content: fmt.Sprintf("Error: old_string matches %d locations in %s. Provide a larger unique context.", count, path),
			IsError: true,
		}
	}

	newContent := strings.Replace(content, oldString, newString, 1)

	absPath, _ := filepath.Abs(path)
	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return ToolResult{Content: fmt.Sprintf("Error writing file: %v", err), IsError: true}
	}

	lastEditedFile = absPath

	var display strings.Builder
	display.WriteString(dim("edited " + path + "  "))
	display.WriteString("\033[2;7m ctrl+e to edit \033[0m")
	display.WriteString("\n")
	display.WriteString(renderDiff(content, newContent, path))
	fmt.Println(display.String())

	return ToolResult{Content: fmt.Sprintf("Successfully edited %s", path), IsError: false}
}

// findNearestMatch finds the most similar substring in content to the target.
// Returns a context snippet around the best match, or "" if nothing close.
func findNearestMatch(content, target string) string {
	lines := strings.Split(content, "\n")
	targetLines := strings.Split(target, "\n")

	if len(targetLines) == 0 {
		return ""
	}

	firstLine := strings.TrimSpace(targetLines[0])
	if firstLine == "" && len(targetLines) > 1 {
		firstLine = strings.TrimSpace(targetLines[1])
	}
	if firstLine == "" {
		return ""
	}

	bestScore := 0
	bestIdx := -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		score := longestCommonSubstring(trimmed, firstLine)
		if score > bestScore && score >= len(firstLine)/3 {
			bestScore = score
			bestIdx = i
		}
	}

	if bestIdx < 0 {
		return ""
	}

	start := bestIdx - 2
	if start < 0 {
		start = 0
	}
	end := bestIdx + len(targetLines) + 2
	if end > len(lines) {
		end = len(lines)
	}

	var snippet strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&snippet, "%4d│ %s\n", i+1, lines[i])
	}
	return snippet.String()
}

// longestCommonSubstring returns the length of the longest common substring.
func longestCommonSubstring(a, b string) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if len(a) > 500 {
		a = a[:500]
	}
	if len(b) > 500 {
		b = b[:500]
	}

	maxLen := 0
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)

	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				curr[j] = prev[j-1] + 1
				if curr[j] > maxLen {
					maxLen = curr[j]
				}
			} else {
				curr[j] = 0
			}
		}
		prev, curr = curr, prev
		for j := range curr {
			curr[j] = 0
		}
	}
	return maxLen
}

func execListFiles(path string) ToolResult {
	entries, err := os.ReadDir(path)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error listing directory: %v", err), IsError: true}
	}

	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return ToolResult{Content: b.String(), IsError: false}
}

func execRegexEdit(pattern, replacement, globPattern, dryRunStr string) ToolResult {
	if pattern == "" {
		return ToolResult{Content: "Error: pattern is required", IsError: true}
	}
	if globPattern == "" {
		return ToolResult{Content: "Error: glob is required", IsError: true}
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error: invalid regex: %v", err), IsError: true}
	}

	dryRun := dryRunStr != "false" // default to dry run for safety

	var files []string
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == ".oc" || base == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		matched, _ := filepath.Match(filepath.Base(globPattern), filepath.Base(path))
		if !matched {
			matched = globMatch(globPattern, path)
		}
		if matched {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return ToolResult{Content: fmt.Sprintf("Error walking directory: %v", err), IsError: true}
	}

	if len(files) == 0 {
		return ToolResult{Content: fmt.Sprintf("No files matched glob pattern: %s", globPattern), IsError: true}
	}

	const maxFiles = 100
	if len(files) > maxFiles {
		return ToolResult{
			Content: fmt.Sprintf("Error: glob matched %d files (max %d). Use a more specific pattern.", len(files), maxFiles),
			IsError: true,
		}
	}

	var report strings.Builder
	totalMatches := 0
	filesChanged := 0

	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)

		matches := re.FindAllStringIndex(content, -1)
		if len(matches) == 0 {
			continue
		}

		newContent := re.ReplaceAllString(content, replacement)
		if newContent == content {
			continue
		}

		totalMatches += len(matches)
		filesChanged++

		if dryRun {
			report.WriteString(fmt.Sprintf("--- %s (%d matches) ---\n", path, len(matches)))
			lines := strings.Split(content, "\n")
			shown := 0
			for lineNo, line := range lines {
				if shown >= 3 {
					if len(matches) > 3 {
						report.WriteString(fmt.Sprintf("  ... and %d more matches\n", len(matches)-3))
					}
					break
				}
				if re.MatchString(line) {
					replaced := re.ReplaceAllString(line, replacement)
					report.WriteString(fmt.Sprintf("  L%d: - %s\n", lineNo+1, strings.TrimSpace(line)))
					report.WriteString(fmt.Sprintf("  L%d: + %s\n", lineNo+1, strings.TrimSpace(replaced)))
					shown++
				}
			}
			report.WriteString("\n")
		} else {
			if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
				report.WriteString(fmt.Sprintf("Error writing %s: %v\n", path, err))
				continue
			}
			report.WriteString(fmt.Sprintf("edited %s (%d replacements)\n", path, len(matches)))
			fmt.Println(dim(fmt.Sprintf("  edited %s (%d replacements)", path, len(matches))))
		}
	}

	if filesChanged == 0 {
		return ToolResult{Content: fmt.Sprintf("No matches found for pattern %q in %d files", pattern, len(files)), IsError: false}
	}

	mode := "DRY RUN"
	if !dryRun {
		mode = "APPLIED"
	}
	summary := fmt.Sprintf("[%s] %d matches across %d files (scanned %d files)\n\n%s", mode, totalMatches, filesChanged, len(files), report.String())
	return ToolResult{Content: summary, IsError: false}
}

// globMatch does a simple glob match supporting ** patterns.
func globMatch(pattern, path string) bool {
	if strings.HasPrefix(pattern, "**/") {
		suffix := pattern[3:]
		parts := strings.Split(path, string(filepath.Separator))
		for i := range parts {
			subpath := strings.Join(parts[i:], string(filepath.Separator))
			if matched, _ := filepath.Match(suffix, subpath); matched {
				return true
			}
			if matched, _ := filepath.Match(suffix, filepath.Base(path)); matched {
				return true
			}
		}
		return false
	}
	matched, _ := filepath.Match(pattern, path)
	return matched
}

// Knowledge holds accumulated project understanding (Tier 1).
// It grows over time as oc is used, reducing the need for the LLM
// to rediscover project structure, conventions, and decisions.
type Knowledge struct {
	Decisions map[string]string `json:"decisions"`

	FileIndex map[string]*FileEntry `json:"file_index"`

	Conventions []Convention `json:"conventions"`

	Preamble string `json:"preamble"`

	TotalInteractions int       `json:"total_interactions"`
	TokensSaved       int       `json:"tokens_saved"` // estimated cumulative savings
	Updated           time.Time `json:"updated"`
}

type FileEntry struct {
	Path     string `json:"path"`
	Outline  string `json:"outline"` // function/type signatures only
	Hash     string `json:"hash"`    // content hash
	Lines    int    `json:"lines"`
	Language string `json:"language"`
}

type Convention struct {
	Type  string `json:"type"`  // "naming", "structure", "test_pattern"
	Rule  string `json:"rule"`  // human-readable
	Count int    `json:"count"` // times observed
}

func NewKnowledge() *Knowledge {
	return &Knowledge{
		Decisions: make(map[string]string),
		FileIndex: make(map[string]*FileEntry),
		Updated:   time.Now(),
	}
}

func LoadKnowledge() *Knowledge {
	base, err := ocBaseDir()
	if err != nil {
		return NewKnowledge()
	}
	data, err := os.ReadFile(filepath.Join(base, "knowledge.json"))
	if err != nil {
		return NewKnowledge()
	}
	var k Knowledge
	if json.Unmarshal(data, &k) != nil {
		return NewKnowledge()
	}
	if k.Decisions == nil {
		k.Decisions = make(map[string]string)
	}
	if k.FileIndex == nil {
		k.FileIndex = make(map[string]*FileEntry)
	}
	return &k
}

func (k *Knowledge) Save() error {
	base, err := ocBaseDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(k, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(base, "knowledge.json"), data, 0o644)
}

// LearnFromTrace extracts knowledge from a completed trace.
func (k *Knowledge) LearnFromTrace(trace *IRTrace) {
	if trace == nil {
		return
	}
	k.TotalInteractions++
	k.Updated = time.Now()

	for _, op := range trace.Ops {
		if op.Tool == "read_file" && !op.IsError {
			var args struct {
				Path string `json:"path"`
			}
			json.Unmarshal(op.Args, &args)
			if args.Path != "" {
				k.updateFileEntry(args.Path, op.Output)
			}
		}

		if op.Tool == "write_file" && !op.IsError {
			var args struct {
				Path string `json:"path"`
			}
			json.Unmarshal(op.Args, &args)
			if args.Path != "" {
				dir := filepath.Dir(args.Path)
				if dir != "." {
					ext := filepath.Ext(args.Path)
					k.recordConvention("structure", fmt.Sprintf("%s files go in %s/", ext, dir))
				}
			}
		}

		if op.Tool == "bash" && !op.IsError {
			var args struct {
				Command string `json:"command"`
			}
			json.Unmarshal(op.Args, &args)
			if classifyBashOp(op.Args) == OpAssert {
				k.Decisions["test_command"] = args.Command
			}
		}
	}

	k.compilePreamble()
}

func (k *Knowledge) updateFileEntry(path string, content string) {
	h := sha256.Sum256([]byte(content))
	hash := fmt.Sprintf("%x", h)[:16]
	if existing, ok := k.FileIndex[path]; ok && existing.Hash == hash {
		return
	}
	k.FileIndex[path] = &FileEntry{
		Path:     path,
		Outline:  Outline(path),
		Hash:     hash,
		Lines:    strings.Count(content, "\n") + 1,
		Language: filepath.Ext(path),
	}
}

func (k *Knowledge) recordConvention(typ, rule string) {
	for i, c := range k.Conventions {
		if c.Type == typ && c.Rule == rule {
			k.Conventions[i].Count++
			return
		}
	}
	k.Conventions = append(k.Conventions, Convention{Type: typ, Rule: rule, Count: 1})
}

// compilePreamble builds a compact string from all knowledge.
func (k *Knowledge) compilePreamble() {
	var b strings.Builder

	for key, val := range k.Decisions {
		b.WriteString(fmt.Sprintf("%s: %s\n", key, val))
	}

	if len(k.FileIndex) > 0 {
		type fi struct {
			path  string
			lines int
		}
		var files []fi
		for _, f := range k.FileIndex {
			files = append(files, fi{f.Path, f.Lines})
		}
		sort.Slice(files, func(i, j int) bool { return files[i].lines > files[j].lines })
		b.WriteString("\nKey files:\n")
		for i, f := range files {
			if i >= 20 {
				break
			}
			b.WriteString(fmt.Sprintf("  %s (%d lines)\n", f.path, f.lines))
		}
	}

	for _, c := range k.Conventions {
		if c.Count >= 3 {
			b.WriteString(fmt.Sprintf("Convention: %s\n", c.Rule))
		}
	}

	k.Preamble = b.String()
}

// AutoBuildResult holds the output of a build/check command.
type AutoBuildResult struct {
	Output  string
	IsError bool
	Elapsed time.Duration
	Command string
}

// CompilerError represents a parsed error from compiler output.
type CompilerError struct {
	File    string
	Line    int
	Col     int
	Message string
}

// ParseCompilerErrors extracts structured errors from build output.
// Supports Go, Rust (cargo), and TypeScript error formats.
func ParseCompilerErrors(output, language string) []CompilerError {
	var errors []CompilerError
	seen := make(map[string]bool) // dedup by file:line

	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if ce := parseErrorLine(line, language); ce != nil {
			key := fmt.Sprintf("%s:%d", ce.File, ce.Line)
			if !seen[key] {
				seen[key] = true
				errors = append(errors, *ce)
			}
		}
	}

	if len(errors) > 5 {
		errors = errors[:5]
	}
	return errors
}

// Go: ./main.go:42:15: undefined: foo
// Rust: error[E0425]: cannot find value `x` in this scope --> src/main.rs:10:5
// TS: src/index.ts(10,5): error TS2304: Cannot find name 'x'.
// Generic: file.go:10:5: message
var (
	goErrorRe   = regexp.MustCompile(`^(.+\.go):(\d+):(\d+):\s*(.+)$`)
	rustErrorRe = regexp.MustCompile(`-->\s*(.+\.rs):(\d+):(\d+)`)
	rustMsgRe   = regexp.MustCompile(`^error(?:\[E\d+\])?:\s*(.+)`)
	tsErrorRe   = regexp.MustCompile(`^(.+\.tsx?)\((\d+),(\d+)\):\s*(.+)$`)
)

func parseErrorLine(line, language string) *CompilerError {
	line = strings.TrimSpace(line)
	if line == "" {
		return nil
	}

	switch language {
	case "go":
		if m := goErrorRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			return &CompilerError{File: m[1], Line: ln, Col: col, Message: m[4]}
		}
	case "rust":
		if m := rustErrorRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			return &CompilerError{File: m[1], Line: ln, Col: col}
		}
	case "node":
		if m := tsErrorRe.FindStringSubmatch(line); m != nil {
			ln, _ := strconv.Atoi(m[2])
			col, _ := strconv.Atoi(m[3])
			return &CompilerError{File: m[1], Line: ln, Col: col, Message: m[4]}
		}
	}

	if m := goErrorRe.FindStringSubmatch(line); m != nil {
		ln, _ := strconv.Atoi(m[2])
		col, _ := strconv.Atoi(m[3])
		return &CompilerError{File: m[1], Line: ln, Col: col, Message: m[4]}
	}

	return nil
}


// readSourceContext reads lines around the target line from a file.
// Returns numbered lines with the error line marked.
func readSourceContext(path string, targetLine, contextLines int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	if targetLine < 1 || targetLine > len(lines) {
		return ""
	}

	start := targetLine - contextLines - 1
	if start < 0 {
		start = 0
	}
	end := targetLine + contextLines
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := start; i < end; i++ {
		lineNum := i + 1
		marker := " "
		if lineNum == targetLine {
			marker = ">"
		}
		b.WriteString(fmt.Sprintf("%s %4d│ %s\n", marker, lineNum, lines[i]))
	}
	return b.String()
}

func execWebfetch(rawURL, maxLenRaw string) ToolResult {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ToolResult{Content: "Error: url is required", IsError: true}
	}
	parsed, err := neturl.Parse(u)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ToolResult{Content: "Error: invalid URL", IsError: true}
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return ToolResult{Content: "Error: only http/https URLs are supported", IsError: true}
	}

	maxLen := 20000
	if n, ok := parsePositiveInt(maxLenRaw); ok {
		maxLen = n
	}
	if maxLen > 50000 {
		maxLen = 50000
	}
	if maxLen < 1 {
		maxLen = 20000
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return ToolResult{Content: "Error: failed to build request", IsError: true}
	}
	req.Header.Set("User-Agent", "oc-webfetch/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return ToolResult{Content: "Error fetching URL: " + err.Error(), IsError: true}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return ToolResult{Content: "Error reading response body: " + err.Error(), IsError: true}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 400 {
			msg = msg[:400] + "..."
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return ToolResult{Content: "Error: HTTP " + resp.Status + ": " + msg, IsError: true}
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	text := string(body)
	if strings.Contains(contentType, "text/html") || strings.Contains(text, "<html") {
		text = htmlToText(text)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ToolResult{Content: "No readable content found.", IsError: false}
	}
	if len(text) > maxLen {
		text = strings.TrimSpace(text[:maxLen]) + "\n\n<tool_metadata>\ntruncated=true\ntool=webfetch\n</tool_metadata>"
	}
	return ToolResult{Content: truncateToolOutput("webfetch", text), IsError: false}
}

func htmlToText(raw string) string {
	doc, err := html.Parse(strings.NewReader(raw))
	if err != nil {
		return raw
	}

	skipTags := map[string]bool{
		"script":   true,
		"style":    true,
		"nav":      true,
		"noscript": true,
	}
	blockTags := map[string]bool{
		"p": true, "div": true, "section": true, "article": true, "main": true,
		"li": true, "ul": true, "ol": true, "h1": true, "h2": true, "h3": true,
		"h4": true, "h5": true, "h6": true, "pre": true, "blockquote": true, "br": true,
	}

	var b strings.Builder
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, skip bool) {
		if n == nil {
			return
		}
		if n.Type == html.ElementNode {
			tag := strings.ToLower(n.Data)
			if skipTags[tag] {
				skip = true
			}
			if blockTags[tag] {
				b.WriteByte('\n')
			}
		}
		if !skip && n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" {
				if b.Len() > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(t)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, skip)
		}
		if n.Type == html.ElementNode && blockTags[strings.ToLower(n.Data)] {
			b.WriteByte('\n')
		}
	}
	walk(doc, false)

	lines := strings.Split(b.String(), "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.Join(strings.Fields(ln), " ")
		if ln != "" {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}
