package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const systemPrompt = `You are oc, a coding agent in the terminal.

You are an interactive CLI tool that helps users with software engineering tasks. Use the instructions below and the tools available to you to assist the user.

IMPORTANT: You must NEVER generate or guess URLs for the user unless you are confident that the URLs are for helping the user with programming. You may use URLs provided by the user in their messages or local files.

# Tone and style
- Only use emojis if the user explicitly requests it. Avoid using emojis in all communication unless asked.
- Your output will be displayed on a command line interface. Your responses should be short and concise. You can use GitHub-flavored markdown for formatting.
- Output text to communicate with the user; all text you output outside of tool use is displayed to the user. Only use tools to complete tasks. Never use tools like bash or code comments as means to communicate with the user during the session.
- Prefer editing existing files over creating new ones when possible, but create new files whenever the task requires it. Do not hesitate or ask for permission to create files.
- Do NOT narrate what you are about to do between tool calls. Do NOT say "Let me read...", "Now let me examine...", "Let me also check...". Just call the tools silently. But DO output substantive content — answers, explanations, analysis, rewrites — when the user asks for it. The rule is: no filler narration between tools, but always produce real output when the task calls for it.

# Professional objectivity
Prioritize technical accuracy and truthfulness over validating the user's beliefs. Focus on facts and problem-solving, providing direct, objective technical info without any unnecessary superlatives, praise, or emotional validation. Whenever there is uncertainty, investigate to find the truth first rather than instinctively confirming the user's beliefs.

# Doing tasks
The user will primarily request you perform software engineering tasks. This includes solving bugs, adding new functionality, refactoring code, explaining code, and more.

- Tool results and user messages may include <system-reminder> tags. <system-reminder> tags contain useful information and reminders. They are automatically added by the system, and bear no direct relation to the specific tool results or user messages in which they appear.

# Tool usage policy
- Use specialized tools instead of bash commands when possible. For file operations, use dedicated tools: read_file/read_files for reading files instead of cat/head/tail, edit_file for editing instead of sed/awk, and write_file/write_files for creating files instead of cat with heredoc or echo redirection. Use grep and find_symbol for content/symbol search. Use list_files for directory inspection. Reserve bash exclusively for actual system commands and terminal operations that require shell execution (build/test/git/runtime).
- NEVER use bash echo or other command-line tools to communicate thoughts, explanations, or instructions to the user. Output all communication directly in your response text instead.
- You can call multiple tools in a single response. If you intend to call multiple tools and there are no dependencies between them, make all independent tool calls in parallel. Maximize use of parallel tool calls where possible to increase efficiency. However, if some tool calls depend on previous calls to inform dependent values, do NOT call these tools in parallel and instead call them sequentially. Never use placeholders or guess missing parameters in tool calls.
- You are running in the repository workspace with direct file-tool access. If file contents are needed, read them with tools; do not ask the user to paste local source files.
- Only run build/test commands after making code changes, not during initial discovery.
- For multi-step tasks, call todowrite early with a concise plan and keep it updated as you complete steps.
- No filler text between consecutive tool calls in the same response. Batch all reads into one read_files call, then act on the results. When your task is to produce text (write docs, explain code, answer questions), you MUST output that text — suppressing output is never correct.
- IMPORTANT: When you need to read multiple files, ALWAYS use read_files with all paths in one call instead of separate read_file calls. Never read files one at a time.
- When making the same mechanical change across multiple files, use regex_edit instead of repeated read_file+edit_file loops.
- For large restructuring of a single file, prefer write_file with the complete new content over many small edit_file calls.
- Prefer read_file with line ranges for large files and avoid repeatedly reading the full file after successful edits.
- Aim for minimum API round-trips: gather all context in round 1, make all changes in round 2, verify in round 3. Three rounds should handle most tasks.
- IMPORTANT: For multi-file search, analysis, or transformation tasks, ALWAYS use the code tool instead of sequential grep/read_file calls. The code tool runs TypeScript with oc.* APIs and completes complex operations in ONE call.
- IMPORTANT: If a run_skill is listed as available in your context, you MUST use it instead of writing new code. Skills are pre-tested and optimized.
- After completing a useful code tool script, save it with save_skill for future reuse.

# Execution behavior
NEVER end your turn without having truly and completely solved the problem, and when you say you are going to make a tool call, make sure you ACTUALLY make the tool call, instead of ending your turn. NEVER claim you made changes to files without actually calling edit_file, write_file, or write_files. Describing changes is not the same as making them.

- If you say you will do a next step, do it in the same turn with concrete tool calls.
- Continue until the task is actually completed or you are genuinely blocked by missing required information.
- Do not ask the user for confirmation, permission, or approval before making changes. Just do the work.
- Do not ask for permission to use tools that are already available; choose the best available tool and continue.

# Editing constraints
- Default to ASCII when editing or creating files. Only introduce non-ASCII when clearly justified and already used by the file.
- Only add comments if necessary to explain non-obvious logic.

# Git and workspace hygiene
- Never revert user changes unless explicitly requested.
- Do not use destructive git commands unless explicitly requested.
- Do not amend commits unless explicitly requested.

# Code references
When referencing specific functions or pieces of code include the pattern file_path:line_number to allow the user to easily navigate to the source code location.`

const codexSystemPrompt = `You are oc, a coding agent in the terminal.

You are an interactive CLI tool that helps users with software engineering tasks. Use the instructions below and the tools available to you to assist the user.

# Editing constraints
- Default to ASCII when editing or creating files. Only introduce non-ASCII when clearly justified and already used by the file.
- Only add comments if necessary to explain non-obvious logic.

# Tool usage
- Prefer specialized tools over shell for file operations:
  - Use read_file/read_files to view files, edit_file to modify files, write_file/write_files to create files.
  - Use grep and find_symbol to search file contents and symbols.
  - Use list_files for directory inspection.
- Use bash for terminal operations (git, builds, tests, running scripts).
- Run tool calls in parallel when neither call needs the other's output; otherwise run sequentially.
- For tasks likely to take multiple tool calls, start with todowrite and maintain it as work progresses.
- Keep narration minimal between tool calls. Prefer action over commentary.
- Use read_file with start_line/end_line for large files and avoid unnecessary full-file rereads.

# Git and workspace hygiene
- NEVER revert existing changes you did not make unless explicitly requested.
- Do not amend commits unless explicitly requested.
- NEVER use destructive commands like git reset --hard or git checkout -- unless specifically requested.

# Presenting your work
- Default: be very concise; friendly coding teammate tone.
- Default: do the work without asking questions. Treat short tasks as sufficient direction; infer missing details by reading the codebase and following existing conventions.
- Questions: only ask when you are truly blocked after checking relevant context AND you cannot safely pick a reasonable default. This usually means one of:
  * The request is ambiguous in a way that materially changes the result and you cannot disambiguate by reading the repo.
  * The action is destructive/irreversible, touches production, or changes billing/security posture.
  * You need a secret/credential/value that cannot be inferred (API key, account id, etc.).
- If you must ask: do all non-blocked work first, then ask exactly one targeted question, include your recommended default, and state what would change based on the answer.
- Never ask permission questions like "Should I proceed?" or "Do you want me to run tests?"; proceed with the most reasonable option and mention what you did.
- For code changes: lead with a quick explanation, then details on where and why. Suggest natural next steps briefly if any.

# Execution behavior
NEVER end your turn without having truly and completely solved the problem, and when you say you are going to make a tool call, make sure you ACTUALLY make the tool call, instead of ending your turn. NEVER claim you made changes to files without actually calling edit_file, write_file, or write_files. Describing changes is not the same as making them.

- If you say you will do a next step, do it in the same turn with concrete tool calls.
- Continue until the task is actually completed or you are genuinely blocked.
- Do not ask the user for confirmation, permission, or approval before making changes. Just do the work.
- When implementation is requested, start making concrete edits immediately. Do not explain what you plan to do — just do it.
- Never claim a task is "too large" or needs to be "staged" as an excuse to avoid doing it. Execute the full implementation in one pass.
- Prefer creating new files over asking permission to create them.

# Code references
When referencing files use inline code: ` + "`file_path:line_number`" + `.`

const codexAddendum = `

# OpenAI/Codex runtime notes
- Be decisive and execution-first: for implementation requests, do not stop at analysis or status checks when edits are required.
- Avoid ending a turn with "already done" unless you verified by direct file inspection in this turn.
- If you have enough context, perform concrete edits before summarizing.
`

type AgentMode string

const (
	ModeBuild AgentMode = "build"
	ModePlan  AgentMode = "plan"
)

var currentMode AgentMode = ModeBuild

type turnGuard struct {
	maxTools             int
	maxDiscoveryPreWrite int
	maxRepeatDiscovery   int
	doomLoopThreshold    int
	maxEditsPerFile      int
	toolCalls            int
	discoveryCalls       int
	hasWrite             bool
	seenCalls            map[string]int
	editCallsPerPath     map[string]int
}

func newTurnGuard(auth *AuthMethod) *turnGuard {
	defaultMaxTools := 36
	defaultDiscoveryBeforeWrite := 8
	if auth != nil && auth.IsOpenAI() {
		defaultMaxTools = 24
		defaultDiscoveryBeforeWrite = 5
	}

	return &turnGuard{
		maxTools:             envInt("OC_MAX_TOOL_CALLS_PER_TURN", defaultMaxTools),
		maxDiscoveryPreWrite: envInt("OC_MAX_DISCOVERY_BEFORE_WRITE", defaultDiscoveryBeforeWrite),
		maxRepeatDiscovery:   envInt("OC_MAX_REPEAT_DISCOVERY", 0),
		doomLoopThreshold:    envInt("OC_DOOM_LOOP_THRESHOLD", 3),
		maxEditsPerFile:      envInt("OC_MAX_EDITS_PER_FILE_PER_TURN", 0),
		seenCalls:            make(map[string]int),
		editCallsPerPath:     make(map[string]int),
	}
}

func (g *turnGuard) checkAndRecord(block ContentBlock) *ToolResult {
	g.toolCalls++
	if g.maxTools > 0 && g.toolCalls > g.maxTools {
		traceLog("[jit] guard: tool-step budget reached (%d/%d)", g.toolCalls, g.maxTools)
		return &ToolResult{
			IsError: true,
			Content: fmt.Sprintf("Tool-step budget reached (%d/%d). Stop making tool calls and return a concise implementation summary with remaining tasks.", g.toolCalls, g.maxTools),
		}
	}

	sig := toolCallSemanticKey(block.Name, block.Input)
	g.seenCalls[sig]++
	if g.doomLoopThreshold > 0 && g.seenCalls[sig] >= g.doomLoopThreshold {
		traceLog("[jit] guard: doom_loop blocked key=%s count=%d", sig, g.seenCalls[sig])
		return &ToolResult{
			IsError: true,
			Content: fmt.Sprintf("Doom-loop guard: repeated identical tool call detected (%d repeats) for %q. Use existing results and switch strategy.", g.seenCalls[sig], block.Name),
		}
	}
	if g.maxRepeatDiscovery > 0 && isDiscoveryToolCall(block.Name, block.Input) && g.seenCalls[sig] > g.maxRepeatDiscovery {
		traceLog("[jit] guard: repeated discovery blocked key=%s count=%d", sig, g.seenCalls[sig])
		return &ToolResult{
			IsError: true,
			Content: "Repeated discovery call blocked. Reuse existing results and proceed to edits/tests instead of repeating identical searches.",
		}
	}

	if isWriteToolCall(block.Name, block.Input) {
		g.hasWrite = true
	}
	if block.Name == "edit_file" && g.maxEditsPerFile > 0 {
		p := filepath.Clean(strings.TrimSpace(extractToolFilePath(block.Name, block.Input)))
		if p != "" {
			g.editCallsPerPath[p]++
			if g.editCallsPerPath[p] > g.maxEditsPerFile {
				traceLog("[jit] guard: repeated edit_file blocked path=%s count=%d", p, g.editCallsPerPath[p])
				return &ToolResult{
					IsError: true,
					Content: fmt.Sprintf("Too many edit_file calls on %s in one turn (%d/%d). Batch remaining changes and perform a single write_file for that path.", p, g.editCallsPerPath[p], g.maxEditsPerFile),
				}
			}
		}
	}

	if g.maxDiscoveryPreWrite > 0 && !g.hasWrite && isDiscoveryToolCall(block.Name, block.Input) {
		g.discoveryCalls++
		if g.discoveryCalls > g.maxDiscoveryPreWrite {
			traceLog("[jit] guard: discovery-before-write budget reached (%d/%d)", g.discoveryCalls, g.maxDiscoveryPreWrite)
			return &ToolResult{
				IsError: true,
				Content: fmt.Sprintf("Discovery budget reached before first edit (%d/%d). Start implementing immediately using current context; do not ask routine clarification questions.", g.discoveryCalls, g.maxDiscoveryPreWrite),
			}
		}
	}

	return nil
}

func normalizeToolInput(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "{}"
	}
	return s
}

func toolCallSemanticKey(name string, input json.RawMessage) string {
	switch name {
	case "read_file":
		var a struct {
			Path      string `json:"path"`
			StartLine string `json:"start_line"`
			EndLine   string `json:"end_line"`
			MaxChars  string `json:"max_chars"`
		}
		_ = json.Unmarshal(input, &a)
		p := filepath.Clean(strings.TrimSpace(extractToolFilePath(name, input)))
		if p == "" {
			p = "."
		}
		return fmt.Sprintf("read_file:%s|%s|%s|%s", p, strings.TrimSpace(a.StartLine), strings.TrimSpace(a.EndLine), strings.TrimSpace(a.MaxChars))
	case "list_files":
		p := filepath.Clean(strings.TrimSpace(extractToolFilePath(name, input)))
		if p == "" {
			p = "."
		}
		return name + ":" + p
	case "grep":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
			Include string `json:"include"`
		}
		_ = json.Unmarshal(input, &a)
		pat := strings.ToLower(strings.TrimSpace(a.Pattern))
		p := filepath.Clean(strings.TrimSpace(a.Path))
		if p == "" {
			p = "."
		}
		inc := strings.ToLower(strings.TrimSpace(a.Include))
		return fmt.Sprintf("grep:%s|%s|%s", pat, p, inc)
	case "glob":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(input, &a)
		pat := strings.ToLower(strings.TrimSpace(a.Pattern))
		p := filepath.Clean(strings.TrimSpace(a.Path))
		if p == "" {
			p = "."
		}
		return fmt.Sprintf("glob:%s|%s", pat, p)
	case "webfetch":
		var a struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &a)
		return "webfetch:" + strings.ToLower(strings.TrimSpace(a.URL))
	case "find_symbol":
		var a struct {
			Symbol  string `json:"symbol"`
			Path    string `json:"path"`
			Backend string `json:"backend"`
			Include string `json:"include"`
		}
		_ = json.Unmarshal(input, &a)
		sym := strings.ToLower(strings.TrimSpace(a.Symbol))
		p := filepath.Clean(strings.TrimSpace(a.Path))
		if p == "" {
			p = "."
		}
		backend := strings.ToLower(strings.TrimSpace(a.Backend))
		inc := strings.ToLower(strings.TrimSpace(a.Include))
		return fmt.Sprintf("find_symbol:%s|%s|%s|%s", sym, p, backend, inc)
	case "read_files":
		var a struct {
			Paths []string `json:"paths"`
		}
		_ = json.Unmarshal(input, &a)
		parts := make([]string, 0, len(a.Paths))
		for _, p := range a.Paths {
			p = filepath.Clean(strings.TrimSpace(p))
			if p != "" {
				parts = append(parts, p)
			}
		}
		return "read_files:" + strings.Join(parts, ",")
	case "write_files":
		var a struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		_ = json.Unmarshal(input, &a)
		parts := make([]string, 0, len(a.Files))
		for _, f := range a.Files {
			p := filepath.Clean(strings.TrimSpace(f.Path))
			if p != "" {
				parts = append(parts, p)
			}
		}
		return "write_files:" + strings.Join(parts, ",")
	case "bash":
		cmd := strings.ToLower(normalizeBashCmd(extractBashCommand(input)))
		return "bash:" + cmd
	default:
		return name + ":" + normalizeToolInput(input)
	}
}

func isDiscoveryToolCall(name string, input json.RawMessage) bool {
	switch name {
	case "list_files", "grep", "find_symbol", "glob", "webfetch":
		return true
	case "bash":
		cmd := extractBashCommand(input)
		return classifyBashOp(input) == OpQuery && !isBuildBash(cmd)
	default:
		return false
	}
}

func isWriteToolCall(name string, input json.RawMessage) bool {
	switch name {
	case "write_file", "write_files", "edit_file":
		return true
	case "bash":
		return isWriteBash(extractBashCommand(input))
	default:
		return false
	}
}

func looksLikeImplementationRequest(input string) bool {
	s := strings.ToLower(strings.TrimSpace(input))
	if s == "" {
		return false
	}
	hints := []string{
		"implement", "add ", "remove ", "refactor", "fix ", "complete", "finish",
		"migrate", "cleanup", "update", "change ", "build ", "wire ", "connect ",
	}
	for _, h := range hints {
		if strings.Contains(s, h) {
			return true
		}
	}
	return false
}

// handleResponse is the core LLM interaction loop.
// preamble is injected into the system prompt (Tier 1+).
// specContext is pre-fetched tool results injected into the user message (Tier 2).
func handleResponse(messages *[]Message, auth *AuthMethod, userInput string, preamble string, specContext *string, jitMeta *JITMeta) (*ResponseStats, error) {
	var liveInputTokens int64
	var liveOutputTokens int64
	liveSpinnerStatus := func() string {
		return fmt.Sprintf(" in:%d out:%d", atomic.LoadInt64(&liveInputTokens), atomic.LoadInt64(&liveOutputTokens))
	}
	stopSpinner := startSpinner(liveSpinnerStatus)
	var interrupted atomic.Bool
	var streamMu sync.Mutex
	var activeStream *StreamReader
	stopInterruptWatcher := startEscInterruptWatcher(func() {
		if interrupted.CompareAndSwap(false, true) {
			streamMu.Lock()
			if activeStream != nil {
				activeStream.Close()
			}
			streamMu.Unlock()
			fmt.Print("\r\n")
		}
	})
	defer stopInterruptWatcher()

	apiCalls := 0
	llmToolCalls := 0
	totalInputTokens := 0
	totalOutputTokens := 0
	totalCacheReadTokens := 0
	totalCacheCreationTokens := 0
	overflowInputTokens := 0
	callbackInputTokens = 0
	callbackOutputTokens = 0
	callbackAPICalls = 0
	startTime := time.Now()
	recorder := NewTraceRecorder(userInput)
	guard := newTurnGuard(auth)
	defaultSoftLimit := 40
	if auth != nil && auth.IsOpenAI() {
		defaultSoftLimit = 20
	}
	softStepLimit := envInt("OC_SOFT_STEP_LIMIT", defaultSoftLimit)
	softLimitInjected := false
	forcedImplementRetry := false

	var cache *SessionCache
	if os.Getenv("OC_NO_CACHE") != "1" {
		cache = NewSessionCache()
	}

	basePrompt := systemPrompt
	if auth != nil && auth.IsOpenAI() {
		basePrompt = systemPrompt + codexAddendum
	}
	sysPrompt := basePrompt + "\n\nRuntime context:\n" + buildRuntimeContext()

	if preamble != "" {
		sysPrompt += "\n\nProject Knowledge:\n" + preamble
	}
	if currentMode == ModePlan {
		sysPrompt += "\n\nPlan mode is active. Do not make edits, writes, or other mutating actions. Restrict yourself to read-only exploration and planning."
	}

	apiMessages := cloneMessages(*messages)
	if currentCompactionSummary != "" && currentCompactionBoundary >= -1 {
		apiMessages = buildPostCompactionMessages(currentCompactionSummary, *messages, currentCompactionBoundary)
	}

	if specContext != nil && *specContext != "" {
		last := len(apiMessages) - 1
		if last >= 0 {
			if str, ok := apiMessages[last].Content.(string); ok {
				apiMessages[last].Content = *specContext + "\nUser request: " + str
			}
		}
	} else if len(*messages) > 2 {
		last := len(apiMessages) - 1
		if last >= 0 {
			if _, ok := apiMessages[last].Content.(string); ok {
				if ctx := ResolveSessionContext(*messages, userInput); ctx != "" {
					apiMessages[last].Content = ctx
				}
			}
		}
	}

	for {
		if softStepLimit > 0 && apiCalls >= softStepLimit && !softLimitInjected {
			softLimitInjected = true
			traceLog("[jit] guard: soft step limit reached (%d), injecting wrap-up", apiCalls)
			wrapUpMsg := Message{Role: "user", Content: fmt.Sprintf(
				"[SYSTEM] You have used %d steps. Finish your current task now — make any final edits or run a final test, then provide a summary. Do not start new exploratory work.",
				apiCalls)}
			apiMessages = append(apiMessages, wrapUpMsg)
		}

		var assistantContent strings.Builder
		var assistantBlocks []ContentBlock

		apiCalls++
		prunedForAPI := pruneOldToolOutputs(apiMessages)
		recorder.RecordAPIRequest(sysPrompt, prunedForAPI)
		stream, err := streamChat(prunedForAPI, auth, sysPrompt)
		if err != nil {
			stopSpinner()
			return nil, err
		}
		streamMu.Lock()
		activeStream = stream
		streamMu.Unlock()

		var currentText strings.Builder
		var currentToolID, currentToolName string
		var currentInputJSON strings.Builder
		var stopReason string

		for {
			event, err := stream.Next()
			if err != nil {
				stream.Close()
				streamMu.Lock()
				if activeStream == stream {
					activeStream = nil
				}
				streamMu.Unlock()
				stopSpinner()
				if interrupted.Load() {
					return nil, fmt.Errorf("interrupted")
				}
				return nil, err
			}
			if event == nil {
				break
			}

			switch event.Type {
			case "text":
				currentText.WriteString(event.Text)
			case "tool_use_start":
				if currentText.Len() > 0 {
					raw := currentText.String()
					assistantContent.WriteString(raw)
					assistantBlocks = append(assistantBlocks, ContentBlock{Type: "text", Text: raw})
					currentText.Reset()
				}
				currentToolID = event.ToolID
				currentToolName = event.ToolName
				currentInputJSON.Reset()
			case "input_json":
				currentInputJSON.WriteString(event.JSON)
			case "block_stop":
				if currentToolID != "" {
					inputRaw := json.RawMessage(currentInputJSON.String())
					if len(inputRaw) == 0 {
						inputRaw = json.RawMessage("{}")
					}
					assistantBlocks = append(assistantBlocks, ContentBlock{
						Type: "tool_use", ID: currentToolID, Name: currentToolName, Input: inputRaw,
					})
					currentToolID = ""
					currentToolName = ""
					currentInputJSON.Reset()
				} else if currentText.Len() > 0 {
					raw := currentText.String()
					assistantContent.WriteString(raw)
					assistantBlocks = append(assistantBlocks, ContentBlock{Type: "text", Text: raw})
					currentText.Reset()
				}
			case "usage":
				totalInputTokens += event.InputTokens
				overflowInputTokens += event.InputTokens
				totalCacheReadTokens += event.CacheReadTokens
				totalCacheCreationTokens += event.CacheCreationTokens
				atomic.AddInt64(&liveInputTokens, int64(event.InputTokens))
			case "message_delta":
				stopReason = event.StopReason
				totalOutputTokens += event.OutputTokens
				atomic.AddInt64(&liveOutputTokens, int64(event.OutputTokens))
			}
		}
		stream.Close()
		streamMu.Lock()
		if activeStream == stream {
			activeStream = nil
		}
		streamMu.Unlock()
		if interrupted.Load() {
			stopSpinner()
			return nil, fmt.Errorf("interrupted")
		}

		if currentText.Len() > 0 {
			raw := currentText.String()
			assistantContent.WriteString(raw)
			assistantBlocks = append(assistantBlocks, ContentBlock{Type: "text", Text: raw})
		}

		stopSpinner()

		if assistantContent.Len() > 0 {
			rendered := renderMarkdown(assistantContent.String())
			lines := strings.Split(rendered, "\n")
			for i, line := range lines {
				if i == 0 {
					fmt.Println("  \033[1;32m•\033[0m" + line)
				} else {
					fmt.Println("    " + line)
				}
			}
		}

		if len(assistantBlocks) == 0 && stopReason != "tool_use" {
			apiMessages = append(apiMessages, Message{
				Role:    "assistant",
				Content: []ContentBlock{{Type: "text", Text: "(empty)"}},
			}, Message{
				Role:    "user",
				Content: "Your previous response was empty. Please produce the requested output now.",
			})
			stopSpinner = startSpinner(liveSpinnerStatus)
			continue
		}

		assistantMsg := Message{Role: "assistant", Content: assistantBlocks}
		*messages = append(*messages, assistantMsg)
		apiMessages = append(apiMessages, assistantMsg)

		if stopReason == "tool_use" {
			var toolResults []ContentBlock
			for _, block := range assistantBlocks {
				if block.Type == "tool_use" {
					llmToolCalls++
					fmt.Println(dim(formatToolCall(block.Name, block.Input)))
					toolStart := time.Now()

					var result ToolResult
					cached := false

					if blocked := guard.checkAndRecord(block); blocked != nil {
						result = *blocked
						recorder.RecordEffect(block.Name, block.Input, result, time.Since(toolStart))
						toolResults = append(toolResults, ContentBlock{
							Type: "tool_result", ToolUseID: block.ID, Content: result.Content,
						})
						continue
					}

					if cache != nil {
						switch block.Name {
						case "read_file":
							path := extractToolFilePath(block.Name, block.Input)
							if path != "" {
								if content, hit := cache.Reads.Get(path); hit {
									args := parseReadFileArgs(block.Input)
									if args.StartLine > 0 || args.EndLine > 0 {
										content = readFileLineRange(content, args.StartLine, args.EndLine)
									}
									if args.MaxChars > 0 && len(content) > args.MaxChars {
										content = strings.TrimRight(content[:args.MaxChars], "\n")
										content += fmt.Sprintf("\n\n<tool_metadata>\ntruncated=true\ntool=read_file\nlimit=max_chars\nmax_chars=%d\n</tool_metadata>", args.MaxChars)
									}
									result = ToolResult{Content: content}
									cached = true
								}
							}
						case "bash":
							cmd := extractBashCommand(block.Input)
							if isReadOnlyBash(cmd) || isBuildBash(cmd) {
								if output, hit := cache.Bash.Get(cmd); hit {
									result = ToolResult{Content: output}
									cached = true
								}
							}
						}
					}

					if !cached {
						result = executeTool(block.Name, block.Input)
					}
					if cache != nil && !cached {
						switch block.Name {
						case "read_file":
							if !result.IsError {
								args := parseReadFileArgs(block.Input)
								if args.StartLine == 0 && args.EndLine == 0 {
									path := extractToolFilePath(block.Name, block.Input)
									if path != "" {
										cache.Reads.Put(path, result.Content)
									}
								}
							}
						case "bash":
							cmd := extractBashCommand(block.Input)
							if (isReadOnlyBash(cmd) || isBuildBash(cmd)) && !result.IsError {
								cache.Bash.Put(cmd, result.Content)
							} else if isWriteBash(cmd) {
								cache.Reads.InvalidateAll()
								cache.Bash.InvalidateAll()
							}
						case "write_file", "write_files", "edit_file":
							paths := extractWritePaths(block.Name, block.Input)
							for _, p := range paths {
								cache.Reads.Invalidate(p)
								cache.Bash.InvalidateDir(filepath.Dir(p))
							}
						case "list_files":
						}
					}

					if block.Name == "read_file" && !result.IsError {
						path := extractToolFilePath(block.Name, block.Input)
						if path != "" {
							if lang := langFromExt(filepath.Ext(path)); lang != "" {
								if fm := BuildFileMap(result.Content, lang); fm != nil {
									result.Content += FormatFileMap(fm)
								}
							}
						}
					}

					recorder.RecordEffect(block.Name, block.Input, result, time.Since(toolStart))
					toolResults = append(toolResults, ContentBlock{
						Type: "tool_result", ToolUseID: block.ID, Content: result.Content,
					})
				}
			}
			toolMsg := Message{Role: "user", Content: toolResults}
			*messages = append(*messages, toolMsg)
			apiMessages = append(apiMessages, toolMsg)
		}

		if isOverflow(overflowInputTokens, auth) {
			fullMessages := cloneMessages(*messages)
			boundaryIdx := compactionBoundaryIndex(fullMessages)
			if boundaryIdx < 0 {
				boundaryIdx = len(fullMessages) - 1
			}
			summary, err := runCompaction(fullMessages, auth, sysPrompt)
			if err != nil {
				return nil, err
			}
			currentCompactionBoundary = boundaryIdx
			currentCompactionSummary = summary
			*messages = append(*messages, Message{
				Role: "assistant",
				Content: []ContentBlock{
					{Type: "text", Text: fmt.Sprintf("[Context compacted at %s]", time.Now().Format(time.RFC3339))},
					{Type: "text", Text: "Compaction summary:\n" + summary},
				},
			})
			apiMessages = buildPostCompactionMessages(summary, fullMessages, boundaryIdx)
			apiMessages = append(apiMessages, Message{Role: "user", Content: "Continue with next steps"})
			overflowInputTokens = 0
			stopSpinner = startSpinner(liveSpinnerStatus)
			continue
		}

		if stopReason == "max_tokens" {
			apiMessages = append(apiMessages, Message{
				Role:    "user",
				Content: "[SYSTEM] Your output was truncated due to length. Continue from where you left off.",
			})
			stopSpinner = startSpinner(liveSpinnerStatus)
			continue
		}

		if stopReason != "tool_use" {
			if !forcedImplementRetry &&
				!guard.hasWrite &&
				looksLikeImplementationRequest(userInput) {
				forcedImplementRetry = true
				apiMessages = append(apiMessages, Message{
					Role:    "user",
					Content: "You described what to do but did not make changes. Call write_file or edit_file now — do not explain, just act.",
				})
				stopSpinner = startSpinner(liveSpinnerStatus)
				continue
			}
			break
		}

		stopSpinner = startSpinner(liveSpinnerStatus)
	}

	gi := totalInputTokens + callbackInputTokens
	go_ := totalOutputTokens + callbackOutputTokens
	costNoCacheUSD := float64(gi+totalCacheReadTokens+totalCacheCreationTokens)*3.0/1e6 + float64(go_)*15.0/1e6
	costUSD := float64(gi)*3.0/1e6 + float64(totalCacheReadTokens)*0.30/1e6 + float64(totalCacheCreationTokens)*3.75/1e6 + float64(go_)*15.0/1e6
	recorder.stats = &TraceStats{
		APICalls:            apiCalls + callbackAPICalls,
		InputTokens:         gi,
		OutputTokens:        go_,
		CacheReadTokens:     totalCacheReadTokens,
		CacheCreationTokens: totalCacheCreationTokens,
		CbAPICalls:          callbackAPICalls,
		CbInTokens:          callbackInputTokens,
		CbOutTokens:         callbackOutputTokens,
		ElapsedMs:           float64(time.Since(startTime).Milliseconds()),
		CostUSD:             costUSD,
		CostNoCacheUSD:      costNoCacheUSD,
		JIT:                 jitMeta,
	}

	trace := recorder.Commit()
	if trace != nil {
		irTrace := CompileIR(userInput, recorder.effects)
		jitEngine.Record(userInput, irTrace)
	}

	elapsed := time.Since(startTime)
	respStats := &ResponseStats{
		APICalls:             apiCalls + callbackAPICalls,
		LLMToolCalls:         llmToolCalls,
		InputTokens:          gi,
		OutputTokens:         go_,
		CallbackAPICalls:     callbackAPICalls,
		CallbackInputTokens:  callbackInputTokens,
		CallbackOutputTokens: callbackOutputTokens,
		ElapsedMs:            float64(elapsed.Milliseconds()),
	}

	if cache != nil {
		if cs := cache.Stats(); cs != "" {
			fmt.Println(dim(cs))
		}
	}

	statsStr := fmt.Sprintf("%d API calls, %d tool calls, %d in / %d out tokens, %s",
		apiCalls+callbackAPICalls, llmToolCalls, gi, go_, elapsed.Round(time.Millisecond))
	fmt.Println(dim(statsStr))

	if err := onlineTrainIntentFromTurn(userInput, llmToolCalls > 0); err != nil && traceJIT {
		traceLog("[jit] online intent train error: %v", err)
	}

	return respStats, nil
}

func modeInstruction() string {
	if currentMode == ModePlan {
		return "Plan mode is active. Do not make edits, writes, or other mutating actions. Restrict yourself to read-only exploration and planning."
	}
	return "Build mode is active. Execute tasks directly. For implementation requests, your first response in a turn must perform concrete tool actions (for example reading relevant files, editing code, running checks) rather than only giving plans. Do not ask the user to choose a starting point when they already asked to proceed; only ask follow-up questions if truly blocked by missing required information. Keep discovery tight and non-redundant: gather minimal context, then edit."
}

func buildRuntimeContext() string {
	cwd, _ := os.Getwd()
	var b strings.Builder
	if cwd != "" {
		b.WriteString("- Current working directory: ")
		b.WriteString(cwd)
	}

	tree := buildCompactTree(".", 3, 200)
	if tree != "" {
		b.WriteString("\n\n- Project files:\n")
		b.WriteString(tree)
	}

	return b.String()
}

func buildCompactTree(root string, maxDepth, maxEntries int) string {
	var b strings.Builder
	count := 0

	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxDepth || count >= maxEntries {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if count >= maxEntries {
				b.WriteString("  ... (truncated)\n")
				return
			}
			name := e.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" ||
				name == "__pycache__" || name == "target" || name == "dist" || name == "build" {
				continue
			}
			rel, _ := filepath.Rel(".", filepath.Join(dir, name))
			if rel == "" {
				rel = filepath.Join(dir, name)
			}
			if e.IsDir() {
				b.WriteString("  " + rel + "/\n")
				count++
				walk(filepath.Join(dir, name), depth+1)
			} else {
				b.WriteString("  " + rel + "\n")
				count++
			}
		}
	}
	walk(root, 0)
	return b.String()
}

func cloneMessages(src []Message) []Message {
	dst := make([]Message, len(src))
	copy(dst, src)
	return dst
}

func resolveAnaphoraInput(input string, messages []Message) string {
	normalized := strings.ToLower(strings.TrimSpace(input))
	if normalized == "" {
		return input
	}

	anchors := []string{
		"again", "run it", "rerun", "do it again", "run again", "retry",
		"run that", "do that", "do that again", "run this again", "run it again",
	}
	isAnaphora := false
	for _, a := range anchors {
		if strings.Contains(normalized, a) {
			isAnaphora = true
			break
		}
	}
	if !isAnaphora {
		return input
	}

	if cmd := lastBashCommand(messages); cmd != "" {
		return "run " + cmd
	}
	if prev := lastUserPrompt(messages); prev != "" && strings.TrimSpace(prev) != strings.TrimSpace(input) {
		return prev
	}
	return input
}

func lastBashCommand(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "assistant" {
			continue
		}
		blocks, ok := msg.Content.([]ContentBlock)
		if !ok {
			continue
		}
		for j := len(blocks) - 1; j >= 0; j-- {
			b := blocks[j]
			if b.Type != "tool_use" || b.Name != "bash" {
				continue
			}
			cmd := strings.TrimSpace(extractBashCommand(b.Input))
			if cmd != "" {
				return cmd
			}
		}
	}
	return ""
}

func lastUserPrompt(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != "user" {
			continue
		}
		if s, ok := msg.Content.(string); ok {
			s = strings.TrimSpace(s)
			if s != "" {
				return s
			}
		}
	}
	return ""
}

type ResponseStats struct {
	APICalls             int     `json:"api_calls"`
	LLMToolCalls         int     `json:"llm_tool_calls"`
	InputTokens          int     `json:"input_tokens"`
	OutputTokens         int     `json:"output_tokens"`
	CallbackAPICalls     int     `json:"callback_api_calls"`
	CallbackInputTokens  int     `json:"callback_input_tokens"`
	CallbackOutputTokens int     `json:"callback_output_tokens"`
	ElapsedMs            float64 `json:"elapsed_ms"`
}

func formatToolCall(name string, inputJSON json.RawMessage) string {
	var input map[string]interface{}
	if err := json.Unmarshal(inputJSON, &input); err != nil {
		return name
	}
	var parts []string
	for key, value := range input {
		if str, ok := value.(string); ok {
			if len(str) > 50 {
				str = str[:47] + "..."
			}
			parts = append(parts, fmt.Sprintf("%s: %q", key, str))
		} else {
			parts = append(parts, fmt.Sprintf("%s: %v", key, value))
		}
	}
	if len(parts) == 0 {
		return name
	}
	return fmt.Sprintf("%s(%s)", name, strings.Join(parts, ", "))
}

type Effect struct {
	Tool     string          `json:"tool"`
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	IsError  bool            `json:"is_error"`
	Duration int64           `json:"duration_ms"`
}

type TraceStats struct {
	APICalls            int      `json:"api_calls"`
	InputTokens         int      `json:"input_tokens"`
	OutputTokens        int      `json:"output_tokens"`
	CacheReadTokens     int      `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int      `json:"cache_creation_tokens,omitempty"`
	CbAPICalls          int      `json:"cb_api_calls,omitempty"`
	CbInTokens          int      `json:"cb_input_tokens,omitempty"`
	CbOutTokens         int      `json:"cb_output_tokens,omitempty"`
	ElapsedMs           float64  `json:"elapsed_ms"`
	CostUSD             float64  `json:"cost_usd"`
	CostNoCacheUSD      float64  `json:"cost_no_cache_usd,omitempty"`
	JIT                 *JITMeta `json:"jit,omitempty"`
}

type JITMeta struct {
	PlannedTier     int     `json:"planned_tier"`
	PlannedTierName string  `json:"planned_tier_name,omitempty"`
	PatternID       string  `json:"pattern_id,omitempty"`
	ScriptName      string  `json:"script_name,omitempty"`
	MatchStrategy   string  `json:"match_strategy,omitempty"`
	MatchConfidence float64 `json:"match_confidence,omitempty"`
	UsedNeedContext bool    `json:"used_need_context,omitempty"`
	ResolvedNeeds   int     `json:"resolved_needs,omitempty"`
	SpeculatedOps   int     `json:"speculated_ops,omitempty"`
	ScriptDeopt     bool    `json:"script_deopt,omitempty"`
}

type APIRequestSnapshot struct {
	SystemPrompt string    `json:"system_prompt"`
	Messages     []Message `json:"messages"`
	Timestamp    time.Time `json:"ts"`
}

type Trace struct {
	ID          string               `json:"id"`
	Trigger     string               `json:"trigger"`
	TriggerID   string               `json:"trigger_id"`
	RawTrigger  string               `json:"raw_trigger"`
	Effects     []Effect             `json:"effects"`
	Verified    bool                 `json:"verified"`
	Stats       *TraceStats          `json:"stats,omitempty"`
	APIRequests []APIRequestSnapshot `json:"api_requests,omitempty"`
	Created     time.Time            `json:"created"`
}

type TraceRecorder struct {
	userInput   string
	trigger     string
	triggerID   string
	effects     []Effect
	stats       *TraceStats
	apiRequests []APIRequestSnapshot
	startTime   time.Time
}

func NewTraceRecorder(userInput string) *TraceRecorder {
	trigger, triggerID := normalizeTrigger(userInput)
	return &TraceRecorder{
		userInput: userInput,
		trigger:   trigger,
		triggerID: triggerID,
		startTime: time.Now(),
	}
}

func (tr *TraceRecorder) RecordAPIRequest(sysPrompt string, messages []Message) {
	tr.apiRequests = append(tr.apiRequests, APIRequestSnapshot{
		SystemPrompt: sysPrompt,
		Messages:     messages,
		Timestamp:    time.Now(),
	})
}

func (tr *TraceRecorder) RecordEffect(tool string, input json.RawMessage, result ToolResult, elapsed time.Duration) {
	tr.effects = append(tr.effects, Effect{
		Tool:     tool,
		Input:    input,
		Output:   result.Content,
		IsError:  result.IsError,
		Duration: elapsed.Milliseconds(),
	})
}

func (tr *TraceRecorder) Commit() *Trace {
	if len(tr.effects) == 0 {
		return nil
	}
	last := tr.effects[len(tr.effects)-1]
	if last.IsError {
		return nil
	}

	b := make([]byte, 8)
	rand.Read(b)

	t := &Trace{
		ID:          hex.EncodeToString(b),
		Trigger:     tr.trigger,
		TriggerID:   tr.triggerID,
		RawTrigger:  strings.ToLower(tr.userInput),
		Effects:     tr.effects,
		Verified:    isVerificationTool(last.Tool, last.Input),
		Stats:       tr.stats,
		APIRequests: tr.apiRequests,
		Created:     time.Now(),
	}
	saveTrace(t)

	return t
}

func isVerificationTool(tool string, input json.RawMessage) bool {
	if tool != "bash" {
		return false
	}
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(input, &args) != nil {
		return false
	}
	cmd := strings.ToLower(args.Command)
	for _, kw := range []string{"test", "lint", "check", "build", "fmt", "vet"} {
		if strings.Contains(cmd, kw) {
			return true
		}
	}
	return false
}

func getTraceDir() (string, error) {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	var base string
	if err == nil {
		base = strings.TrimSpace(string(root))
	} else {
		base, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	dir := filepath.Join(base, ".oc", "traces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

func saveTrace(t *Trace) error {
	dir, err := getTraceDir()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, t.TriggerID+".json"), data, 0o644)
}

const (
	defaultContextLimitTokens = 200000
	compactionToolPruneFloor  = 20000
	compactionToolPruneMin    = 20000
	compactionToolPruneKeep   = 40000
	oldToolResultPlaceholder  = "[Old tool result content cleared]"
)

var protectedPrunedTools = map[string]bool{
	"skill": true,
}

func getContextLimit(auth *AuthMethod) int {
	model := ""
	if auth != nil && auth.IsOpenAI() {
		model = strings.ToLower(strings.TrimSpace(openAIModelName()))
	} else {
		model = strings.ToLower(strings.TrimSpace(anthropicModel))
	}

	limits := map[string]int{
		"gpt-5.3-codex":             200000,
		"claude-sonnet-4-20250514":  200000,
		"claude-haiku-4-5-20251001": 200000,
	}
	if lim, ok := limits[model]; ok && lim > 0 {
		return lim
	}
	return defaultContextLimitTokens
}

func isOverflow(inputTokens int, auth *AuthMethod) bool {
	limit := getContextLimit(auth)
	buffer := maxTokens
	if buffer > 20000 {
		buffer = 20000
	}
	if buffer < 1 {
		buffer = 1
	}
	return inputTokens >= limit-buffer
}

func runCompaction(messages []Message, auth *AuthMethod, sysPrompt string) (string, error) {
	summaryPrompt := `Summarize the conversation so far for continuation by the same coding agent.

Return plain text with exactly these sections:
Goal
Instructions
Discoveries
Accomplished
Relevant Files

Rules:
- Be specific and concise.
- Preserve concrete constraints, decisions, and unresolved work.
- Include exact file paths when known.
- Do not include chain-of-thought.
- If a section has no content, write "None".`

	req := cloneMessages(messages)
	req = append(req, Message{Role: "user", Content: summaryPrompt})

	out, err := compactionChat(req, auth, sysPrompt)
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", fmt.Errorf("empty compaction summary")
	}
	return out, nil
}

func buildPostCompactionMessages(summary string, allMessages []Message, boundaryIdx int) []Message {
	post := make([]Message, 0, len(allMessages)+1)
	post = append(post, Message{
		Role:    "assistant",
		Content: "Compaction summary:\n" + strings.TrimSpace(summary),
	})
	if boundaryIdx < -1 {
		boundaryIdx = -1
	}
	if boundaryIdx+1 < len(allMessages) {
		post = append(post, cloneMessages(allMessages[boundaryIdx+1:])...)
	}
	return post
}

func pruneOldToolOutputs(messages []Message) []Message {
	out := cloneMessages(messages)
	total := 0
	for _, msg := range out {
		total += estimateMessageTokens(msg)
	}
	if total < compactionToolPruneFloor {
		return out
	}

	toolByID := map[string]string{}
	for _, msg := range out {
		if msg.Role != "assistant" {
			continue
		}
		blocks, ok := msg.Content.([]ContentBlock)
		if !ok {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && strings.TrimSpace(b.ID) != "" {
				toolByID[b.ID] = strings.ToLower(strings.TrimSpace(b.Name))
			}
		}
	}

	type pruneTarget struct {
		msgIdx   int
		blockIdx int
		tokens   int
	}

	toolTokensSeen := 0
	toolTokensPruned := 0
	userTurnsSeen := 0
	var targets []pruneTarget

loop:
	for i := len(out) - 1; i >= 0; i-- {
		msg := out[i]
		if msg.Role == "assistant" {
			if blocks, ok := msg.Content.([]ContentBlock); ok {
				for _, b := range blocks {
					if b.Type == "text" && strings.Contains(b.Text, "Compaction summary:") {
						break loop
					}
				}
			}
		}

		if msg.Role == "user" {
			if _, ok := msg.Content.(string); ok {
				userTurnsSeen++
			}
		}
		if userTurnsSeen < 2 {
			continue
		}

		if msg.Role != "user" {
			continue
		}
		blocks, ok := msg.Content.([]ContentBlock)
		if !ok || len(blocks) == 0 {
			continue
		}

		for j := len(blocks) - 1; j >= 0; j-- {
			b := blocks[j]
			if b.Type != "tool_result" {
				continue
			}
			content := strings.TrimSpace(b.Content)
			if content == "" || content == oldToolResultPlaceholder {
				continue
			}
			if protectedPrunedTools[toolByID[b.ToolUseID]] {
				continue
			}
			estimate := estimateTokens(content)
			toolTokensSeen += estimate
			if toolTokensSeen > compactionToolPruneKeep {
				toolTokensPruned += estimate
				targets = append(targets, pruneTarget{
					msgIdx:   i,
					blockIdx: j,
					tokens:   estimate,
				})
			}
		}
	}

	if toolTokensPruned < compactionToolPruneMin || len(targets) == 0 {
		return out
	}

	mutated := map[int][]ContentBlock{}
	for _, t := range targets {
		blocks, ok := mutated[t.msgIdx]
		if !ok {
			src, ok := out[t.msgIdx].Content.([]ContentBlock)
			if !ok || len(src) == 0 {
				continue
			}
			blocks = make([]ContentBlock, len(src))
			copy(blocks, src)
			mutated[t.msgIdx] = blocks
		}
		if t.blockIdx >= 0 && t.blockIdx < len(blocks) {
			blocks[t.blockIdx].Content = oldToolResultPlaceholder
		}
	}
	for idx, blocks := range mutated {
		out[idx].Content = blocks
	}

	return out
}

func estimateMessageTokens(msg Message) int {
	switch c := msg.Content.(type) {
	case string:
		return estimateTokens(c)
	case []ContentBlock:
		total := 0
		for _, b := range c {
			total += estimateTokens(b.Type)
			total += estimateTokens(b.Text)
			total += estimateTokens(b.ID)
			total += estimateTokens(b.Name)
			total += estimateTokens(b.ToolUseID)
			total += estimateTokens(b.Content)
			if len(b.Input) > 0 {
				total += estimateTokens(string(b.Input))
			}
		}
		return total
	default:
		raw, _ := json.Marshal(c)
		return estimateTokens(string(raw))
	}
}

func compactionBoundaryIndex(messages []Message) int {
	userTurns := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			continue
		}
		if _, ok := messages[i].Content.(string); !ok {
			continue
		}
		userTurns++
		if userTurns >= 2 {
			return i
		}
	}
	return -1
}

// SessionCache wraps ReadCache and BashMemo for a single conversation turn.
type SessionCache struct {
	Reads *ReadCache
	Bash  *BashMemo
}

// NewSessionCache creates a fresh session cache for one handleResponse turn.
func NewSessionCache() *SessionCache {
	return &SessionCache{
		Reads: NewReadCache(),
		Bash:  NewBashMemo(),
	}
}

// Stats returns a one-line summary of cache performance.
func (sc *SessionCache) Stats() string {
	rh, rt := sc.Reads.hits, sc.Reads.hits+sc.Reads.misses
	bh, bt := sc.Bash.hits, sc.Bash.hits+sc.Bash.misses
	saved := sc.Reads.tokensSaved + sc.Bash.tokensSaved
	if rt == 0 && bt == 0 {
		return ""
	}
	return fmt.Sprintf("[cache] reads: %d hits / %d total, bash: %d hits / %d total, ~%d tokens saved",
		rh, rt, bh, bt, saved)
}

type cacheEntry struct {
	content string
	modTime int64 // UnixNano
	size    int64
	tokens  int
}

type ReadCache struct {
	mu          sync.Mutex
	entries     map[string]*cacheEntry
	hits        int
	misses      int
	tokensSaved int
}

func NewReadCache() *ReadCache {
	return &ReadCache{entries: make(map[string]*cacheEntry)}
}

// Get checks if the file is cached and still valid (mtime+size match).
// Returns (content, true) on cache hit, ("", false) on miss.
func (rc *ReadCache) Get(path string) (string, bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		rc.misses++
		return "", false
	}

	entry, ok := rc.entries[abs]
	if !ok {
		rc.misses++
		return "", false
	}

	info, err := os.Stat(abs)
	if err != nil {
		delete(rc.entries, abs)
		rc.misses++
		return "", false
	}

	if info.ModTime().UnixNano() != entry.modTime || info.Size() != entry.size {
		delete(rc.entries, abs)
		rc.misses++
		return "", false
	}

	rc.hits++
	rc.tokensSaved += entry.tokens
	return entry.content, true
}

// Put stores file content in the cache.
func (rc *ReadCache) Put(path, content string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}

	info, err := os.Stat(abs)
	if err != nil {
		return
	}

	rc.entries[abs] = &cacheEntry{
		content: content,
		modTime: info.ModTime().UnixNano(),
		size:    info.Size(),
		tokens:  estimateTokens(content),
	}
}

// Invalidate removes a file from the cache (called on write/edit).
func (rc *ReadCache) Invalidate(path string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	abs, _ := filepath.Abs(path)
	delete(rc.entries, abs)
}

// InvalidateAll clears the entire read cache.
func (rc *ReadCache) InvalidateAll() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.entries = make(map[string]*cacheEntry)
}

const bashMemoMaxEntries = 50

type BashMemo struct {
	mu          sync.Mutex
	entries     map[string]string // normalized cmd → output
	dirtyDirs   map[string]bool   // dirs modified by writes
	order       []string          // insertion order for LRU eviction
	hits        int
	misses      int
	tokensSaved int
}

func NewBashMemo() *BashMemo {
	return &BashMemo{
		entries:   make(map[string]string),
		dirtyDirs: make(map[string]bool),
	}
}

// Get returns cached output for a read-only command, if available.
func (bm *BashMemo) Get(cmd string) (string, bool) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	norm := normalizeBashCmd(cmd)
	result, ok := bm.entries[norm]
	if !ok {
		bm.misses++
		return "", false
	}

	bm.hits++
	bm.tokensSaved += estimateTokens(result)
	return result, true
}

// Put stores the output of a read-only command.
func (bm *BashMemo) Put(cmd, output string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	norm := normalizeBashCmd(cmd)

	if _, exists := bm.entries[norm]; !exists {
		bm.order = append(bm.order, norm)
		if len(bm.order) > bashMemoMaxEntries {
			evict := bm.order[0]
			bm.order = bm.order[1:]
			delete(bm.entries, evict)
		}
	}

	bm.entries[norm] = output
}

// InvalidateDir evicts all cached commands whose working directory matches.
func (bm *BashMemo) InvalidateDir(dir string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	abs, _ := filepath.Abs(dir)
	bm.dirtyDirs[abs] = true

	bm.entries = make(map[string]string)
	bm.order = nil
}

// InvalidateAll clears the entire bash memo.
func (bm *BashMemo) InvalidateAll() {
	bm.mu.Lock()
	defer bm.mu.Unlock()
	bm.entries = make(map[string]string)
	bm.dirtyDirs = make(map[string]bool)
	bm.order = nil
}

// readOnlyPrefixes are the command prefixes that are safe to cache.
var readOnlyPrefixes = []string{
	"ls", "find", "grep", "rg", "cat", "head", "tail", "wc",
	"git log", "git status", "git diff", "git show", "git branch",
	"tree", "file", "stat", "which", "echo", "pwd", "env",
	"go doc", "go list", "go version", "go env",
}

// isReadOnlyBash returns true if the command is safe to cache (read-only).
func isReadOnlyBash(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)

	for _, op := range []string{">", ">>", "|", "&&", "||", ";", "`", "$(", "tee "} {
		if strings.Contains(trimmed, op) {
			if op == "|" && isReadOnlyPipeline(trimmed) {
				continue
			}
			return false
		}
	}

	for _, prefix := range readOnlyPrefixes {
		if trimmed == prefix || strings.HasPrefix(trimmed, prefix+" ") {
			return true
		}
	}
	return false
}

// isReadOnlyPipeline checks if a pipeline only contains read-only commands.
func isReadOnlyPipeline(cmd string) bool {
	parts := strings.Split(cmd, "|")
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		isRO := false
		for _, prefix := range readOnlyPrefixes {
			if trimmed == prefix || strings.HasPrefix(trimmed, prefix+" ") {
				isRO = true
				break
			}
		}
		if !isRO {
			return false
		}
	}
	return true
}

// buildCmdPrefixes are build/test commands whose output can be cached
// until the next edit/write. Unlike read-only commands, these DO modify
// the filesystem (build artifacts) but their stdout output is cacheable.
var buildCmdPrefixes = []string{
	"cargo build", "cargo check", "cargo test", "cargo b",
	"go build", "go test", "go vet",
	"deno test", "deno check",
	"npm test", "npm run build", "npm run test",
	"nix build", "nix develop",
	"make", "cmake",
}

// isBuildBash returns true if the command is a build/test command
// whose result can be cached until files change.
func isBuildBash(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, prefix := range buildCmdPrefixes {
		if trimmed == prefix || strings.HasPrefix(trimmed, prefix+" ") || strings.HasPrefix(trimmed, prefix+"\n") {
			return true
		}
	}
	return false
}

// isWriteBash returns true if the bash command likely modifies the filesystem.
var writeBashIndicators = []string{
	">", ">>", "tee ", "mv ", "cp ", "rm ", "sed -i", "chmod ", "mkdir ",
	"touch ", "git add", "git commit", "git push", "git checkout",
	"go build", "go install", "npm install", "pip install",
}

func isWriteBash(cmd string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, indicator := range writeBashIndicators {
		if strings.Contains(trimmed, indicator) {
			return true
		}
	}
	return false
}

// normalizeBashCmd trims whitespace and collapses internal whitespace.
func normalizeBashCmd(cmd string) string {
	fields := strings.Fields(cmd)
	return strings.Join(fields, " ")
}

// extractToolFilePath extracts the file path from a tool's JSON input.
func extractToolFilePath(toolName string, input json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	if p, ok := m["path"].(string); ok {
		return p
	}
	if p, ok := m["file_path"].(string); ok {
		return p
	}
	return ""
}

// extractBashCommand extracts the command string from bash tool input.
func extractBashCommand(input json.RawMessage) string {
	var m map[string]string
	if err := json.Unmarshal(input, &m); err != nil {
		return ""
	}
	return m["command"]
}

// extractWritePaths returns file paths affected by a write/edit tool call.
func extractWritePaths(toolName string, input json.RawMessage) []string {
	if toolName == "write_files" {
		var a struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		if err := json.Unmarshal(input, &a); err == nil {
			paths := make([]string, 0, len(a.Files))
			for _, f := range a.Files {
				p := strings.TrimSpace(f.Path)
				if p != "" {
					paths = append(paths, p)
				}
			}
			return paths
		}
	}
	p := extractToolFilePath(toolName, input)
	if p != "" {
		return []string{p}
	}
	return nil
}

// estimateTokens is defined in slicer.go

// FileMap is a lightweight structural map of a source file,
// listing key code boundaries (functions, types, imports) with line ranges.
// This helps the LLM target edits precisely without needing to re-read.
type FileMap struct {
	Symbols []FileSymbol
}

type FileSymbol struct {
	Kind string // "func", "type", "import", "class", "method", "const", "var"
	Name string
	Line int // 1-based start line
	End  int // 1-based end line (approximate)
}

// BuildFileMap extracts structural symbols from source code.
// Supports Go, Rust, TypeScript/JavaScript, and Python.
func BuildFileMap(content, language string) *FileMap {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 {
		return nil
	}

	var symbols []FileSymbol

	switch language {
	case "go":
		symbols = extractGoSymbols(lines)
	case "rust":
		symbols = extractRustSymbols(lines)
	case "node":
		symbols = extractTSSymbols(lines)
	case "python":
		symbols = extractPythonSymbols(lines)
	default:
		return nil
	}

	if len(symbols) == 0 {
		return nil
	}

	return &FileMap{Symbols: symbols}
}

// FormatFileMap produces a compact text representation for injection.
func FormatFileMap(fm *FileMap) string {
	if fm == nil || len(fm.Symbols) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n[file map]\n")
	for _, s := range fm.Symbols {
		if s.End > 0 && s.End != s.Line {
			b.WriteString(fmt.Sprintf("  %s %s: L%d-%d\n", s.Kind, s.Name, s.Line, s.End))
		} else {
			b.WriteString(fmt.Sprintf("  %s %s: L%d\n", s.Kind, s.Name, s.Line))
		}
	}
	return b.String()
}

var (
	goFuncRe   = regexp.MustCompile(`^func\s+(?:\(.*?\)\s+)?(\w+)`)
	goTypeRe   = regexp.MustCompile(`^type\s+(\w+)`)
	goImportRe = regexp.MustCompile(`^import\s+`)
	goConstRe  = regexp.MustCompile(`^(?:const|var)\s+(?:\(\s*)?(\w+)?`)
)

func extractGoSymbols(lines []string) []FileSymbol {
	var symbols []FileSymbol
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := goFuncRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "func", Name: m[1], Line: i + 1, End: end})
		} else if m := goTypeRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "type", Name: m[1], Line: i + 1, End: end})
		} else if goImportRe.MatchString(trimmed) {
			end := i + 1
			if strings.Contains(trimmed, "(") {
				end = findClosingParen(lines, i)
			}
			symbols = append(symbols, FileSymbol{Kind: "import", Name: "", Line: i + 1, End: end})
		}
	}
	return symbols
}

var (
	rustFnRe     = regexp.MustCompile(`^(?:pub\s+)?(?:async\s+)?fn\s+(\w+)`)
	rustStructRe = regexp.MustCompile(`^(?:pub\s+)?(?:struct|enum|trait)\s+(\w+)`)
	rustImplRe   = regexp.MustCompile(`^(?:pub\s+)?impl(?:<.*?>)?\s+(\w+)`)
	rustUseRe    = regexp.MustCompile(`^(?:pub\s+)?use\s+`)
)

func extractRustSymbols(lines []string) []FileSymbol {
	var symbols []FileSymbol
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := rustFnRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "func", Name: m[1], Line: i + 1, End: end})
		} else if m := rustStructRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "type", Name: m[1], Line: i + 1, End: end})
		} else if m := rustImplRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "impl", Name: m[1], Line: i + 1, End: end})
		}
	}
	return symbols
}

var (
	tsFuncRe   = regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s+(\w+)`)
	tsClassRe  = regexp.MustCompile(`^(?:export\s+)?class\s+(\w+)`)
	tsArrowRe  = regexp.MustCompile(`^(?:export\s+)?(?:const|let)\s+(\w+)\s*=\s*(?:async\s+)?\(`)
	tsImportRe = regexp.MustCompile(`^import\s+`)
	tsMethodRe = regexp.MustCompile(`^\s+(?:async\s+)?(\w+)\s*\(`)
)

func extractTSSymbols(lines []string) []FileSymbol {
	var symbols []FileSymbol
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := tsFuncRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "func", Name: m[1], Line: i + 1, End: end})
		} else if m := tsClassRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "class", Name: m[1], Line: i + 1, End: end})
		} else if m := tsArrowRe.FindStringSubmatch(trimmed); m != nil {
			end := findBlockEnd(lines, i)
			symbols = append(symbols, FileSymbol{Kind: "func", Name: m[1], Line: i + 1, End: end})
		} else if tsImportRe.MatchString(trimmed) {
			symbols = append(symbols, FileSymbol{Kind: "import", Name: "", Line: i + 1})
		}
	}
	return symbols
}

var (
	pyFuncRe   = regexp.MustCompile(`^def\s+(\w+)`)
	pyClassRe  = regexp.MustCompile(`^class\s+(\w+)`)
	pyImportRe = regexp.MustCompile(`^(?:import|from)\s+`)
)

func extractPythonSymbols(lines []string) []FileSymbol {
	var symbols []FileSymbol
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		if m := pyFuncRe.FindStringSubmatch(trimmed); m != nil {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			end := findPythonBlockEnd(lines, i, indent)
			kind := "func"
			if indent > 0 {
				kind = "method"
			}
			symbols = append(symbols, FileSymbol{Kind: kind, Name: m[1], Line: i + 1, End: end})
		} else if m := pyClassRe.FindStringSubmatch(trimmed); m != nil {
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			end := findPythonBlockEnd(lines, i, indent)
			symbols = append(symbols, FileSymbol{Kind: "class", Name: m[1], Line: i + 1, End: end})
		}
	}
	return symbols
}

// findBlockEnd finds the closing brace for a block starting at line idx.
// Works for Go, Rust, TS/JS.
func findBlockEnd(lines []string, idx int) int {
	depth := 0
	for i := idx; i < len(lines); i++ {
		for _, ch := range lines[i] {
			if ch == '{' {
				depth++
			} else if ch == '}' {
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
	}
	return idx + 1
}

// findClosingParen finds the closing ) for an import block.
func findClosingParen(lines []string, idx int) int {
	for i := idx; i < len(lines); i++ {
		if strings.Contains(lines[i], ")") {
			return i + 1
		}
	}
	return idx + 1
}

// findPythonBlockEnd finds the end of a Python block by indentation.
func findPythonBlockEnd(lines []string, idx, startIndent int) int {
	for i := idx + 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent <= startIndent {
			return i // line before this one is the last line of the block
		}
	}
	return len(lines) // block extends to end of file
}

type TodoItem struct {
	ID       string `json:"id"`
	Content  string `json:"content"`
	Status   string `json:"status"`
	Priority string `json:"priority"`
}

var currentTodos []TodoItem

func execTodoWrite(inputJSON json.RawMessage) ToolResult {
	var payload struct {
		Todos []TodoItem `json:"todos"`
	}
	if err := json.Unmarshal(inputJSON, &payload); err != nil || payload.Todos == nil {
		var direct []TodoItem
		if err2 := json.Unmarshal(inputJSON, &direct); err2 == nil {
			payload.Todos = direct
		}
	}

	normalized := make([]TodoItem, 0, len(payload.Todos))
	for i, t := range payload.Todos {
		item := TodoItem{
			ID:       strings.TrimSpace(t.ID),
			Content:  strings.TrimSpace(t.Content),
			Status:   strings.ToLower(strings.TrimSpace(t.Status)),
			Priority: strings.ToLower(strings.TrimSpace(t.Priority)),
		}
		if item.ID == "" {
			item.ID = fmt.Sprintf("todo-%d", i+1)
		}
		if item.Content == "" {
			return ToolResult{Content: fmt.Sprintf("Error: todo %q has empty content", item.ID), IsError: true}
		}
		switch item.Status {
		case "", "pending":
			item.Status = "pending"
		case "in_progress", "completed", "cancelled":
		default:
			return ToolResult{Content: fmt.Sprintf("Error: todo %q has invalid status %q", item.ID, item.Status), IsError: true}
		}
		switch item.Priority {
		case "", "medium":
			item.Priority = "medium"
		case "high", "low":
		default:
			return ToolResult{Content: fmt.Sprintf("Error: todo %q has invalid priority %q", item.ID, item.Priority), IsError: true}
		}
		normalized = append(normalized, item)
	}

	currentTodos = normalized
	displayTodos()
	return ToolResult{Content: fmt.Sprintf("Updated %d todos.", len(currentTodos)), IsError: false}
}

func displayTodos() {
	if len(currentTodos) == 0 {
		fmt.Println(dim("No todos."))
		return
	}

	todos := cloneTodos(currentTodos)
	sort.SliceStable(todos, func(i, j int) bool {
		pi := todoPriorityRank(todos[i].Priority)
		pj := todoPriorityRank(todos[j].Priority)
		if pi != pj {
			return pi < pj
		}
		return todos[i].ID < todos[j].ID
	})

	fmt.Println(dim("Todos:"))
	for _, t := range todos {
		fmt.Printf("  %s [%s] (%s) %s\n", todoStatusIcon(t.Status), t.ID, t.Priority, t.Content)
	}
}

func cloneTodos(src []TodoItem) []TodoItem {
	dst := make([]TodoItem, len(src))
	copy(dst, src)
	return dst
}

func todoStatusIcon(status string) string {
	switch status {
	case "completed":
		return "[x]"
	case "in_progress":
		return "[~]"
	case "cancelled":
		return "[-]"
	default:
		return "[ ]"
	}
}

func todoPriorityRank(priority string) int {
	switch priority {
	case "high":
		return 0
	case "medium":
		return 1
	default:
		return 2
	}
}
