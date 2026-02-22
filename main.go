package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/glamour"
	"github.com/pmezard/go-difflib/difflib"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

var currentSessionID string
var currentCompactionBoundary = -1
var currentCompactionSummary string
var traceJIT bool

var jitEngine *JIT

func envInt(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func traceLog(format string, args ...any) {
	if traceJIT {
		fmt.Fprintln(os.Stderr, dim(fmt.Sprintf(format, args...)))
	}
}

func main() {
	var sessionID string

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--trace-jit":
			traceJIT = true
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}

	initPermissions()
	args = ApplyPermissionFlags(args)

	jitEngine = NewJIT()
	traceLog("[jit] %s", jitEngine.Stats())

	if len(args) > 0 {
		switch args[0] {
		case "train-intent":
			if err := runTrainIntentCommand(args[1:]); err != nil {
				fmt.Fprintf(os.Stderr, "train-intent failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "login":
			loginProvider := strings.ToLower(strings.TrimSpace(os.Getenv("OC_PROVIDER")))
			if len(args) > 1 {
				loginProvider = strings.ToLower(strings.TrimSpace(args[1]))
			}
			if loginProvider == "openai" {
				if err := loginOpenAI(); err != nil {
					fmt.Fprintf(os.Stderr, "openai login failed: %v\n", err)
					os.Exit(1)
				}
				return
			}
			if _, err := login(); err != nil {
				fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
				os.Exit(1)
			}
			return
		case "sessions", "ls":
			if err := listSessions(); err != nil {
				fmt.Fprintf(os.Stderr, "error listing sessions: %v\n", err)
				os.Exit(1)
			}
			return
		case "resume", "r":
			if len(args) >= 2 {
				sessionID = args[1]
			} else {
				cwd, _ := os.Getwd()
				autoID, err := findLatestSessionForCWD(cwd)
				if err != nil {
					fmt.Fprintf(os.Stderr, "resume failed: %v\n", err)
					os.Exit(1)
				}
				sessionID = autoID
				fmt.Fprintf(os.Stderr, "auto-resuming latest session for cwd: %s\n", sessionID)
			}
		case "run":
			if len(args) < 2 {
				fmt.Fprintln(os.Stderr, "usage: oc run \"<prompt>\"")
				os.Exit(1)
			}
			prompt := strings.Join(args[1:], " ")
			auth, err := getAuth()
			if err != nil {
				fmt.Fprintf(os.Stderr, "auth error: %v\n", err)
				os.Exit(1)
			}
			if auth == nil {
				fmt.Fprintln(os.Stderr, "No authentication found. Use `oc login` (Anthropic) or set OPENAI_API_KEY.")
				os.Exit(1)
			}

			stats := runWithJIT(prompt, auth)
			statsJSON, _ := json.Marshal(stats)
			fmt.Fprintln(os.Stderr, string(statsJSON))
			os.Exit(0)

		case "help", "--help", "-h":
			fmt.Println("oc — terminal coding-agent CLI\n\nUsage:\n  oc                           Start new interactive chat\n  oc resume [id]               Resume a saved session (or latest for cwd)\n  oc sessions                  List saved sessions\n  oc login                     Authenticate for active/default provider\n  oc login openai              OpenAI login (browser OAuth, device flow, or API key)\n  oc login anthropic           Authenticate with Anthropic OAuth\n  oc run <prompt>              Run a single prompt non-interactively\n  oc train-intent [options]    Train intent logreg model from history\n  oc help                      Show this help\n\nInteractive mode commands:\n  /mode                        Show current mode\n  /mode build                  Build mode (normal implementation)\n  /mode plan                   Plan mode (read-only)\n  /build                       Shortcut for /mode build\n  /plan                        Shortcut for /mode plan\n  /todos                       Show current todos\n\nTrain-intent options:\n  --claude-dir <path>          Claude data dir (default: ~/.claude)\n  --max-samples <n>            Max training samples (default: 4000)\n\nFlags:\n  --trace-jit                  Show JIT optimization trace on stderr\n  --allow-all                  Allow all tool permissions\n  --deny-net                   Deny network access (webfetch)\n  --allow-bash                 Allow bash without prompting\n  --allow-write                Allow file writes without prompting\n  --allow-read=path,...         Scope read to specific paths\n  --allow-bash=go,cargo        Scope bash to specific commands\n\nEnvironment:\n  OC_PROVIDER               Force provider: anthropic|openai (optional)\n  ANTHROPIC_API_KEY         Anthropic API key auth (alternative to OAuth)\n  OPENAI_API_KEY            OpenAI API key auth\n  OPENAI_MODEL              OpenAI model override (default: gpt-5.1-codex-mini)\n  OC_INTENT_ONLINE_TRAIN    Enable online intent learning (1/true)")
			return
		}
	}

	auth, err := getAuth()
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth error: %v\n", err)
		os.Exit(1)
	}
	if auth == nil {
		if strings.EqualFold(strings.TrimSpace(os.Getenv("OC_PROVIDER")), "openai") {
			fmt.Fprintln(os.Stderr, "No OpenAI authentication found. Running OpenAI login...")
			if err := loginOpenAI(); err != nil {
				fmt.Fprintf(os.Stderr, "openai login failed: %v\n", err)
				os.Exit(1)
			}
			auth, err = getAuth()
			if err != nil || auth == nil {
				fmt.Fprintln(os.Stderr, "No OpenAI authentication found after login.")
				os.Exit(1)
			}
		} else {
			fmt.Fprintln(os.Stderr, "No authentication found. Running Anthropic login (or set OPENAI_API_KEY / OC_PROVIDER=openai)...")
			token, err := login()
			if err != nil {
				fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
				os.Exit(1)
			}
			auth = &AuthMethod{Provider: "anthropic", Token: token}
		}
	}

	var messages []Message
	currentSessionID = sessionID

	if sessionID != "" {
		loadedMessages, err := loadSession(sessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error loading session: %v\n", err)
			os.Exit(1)
		}
		messages = loadedMessages
		fmt.Println(dim("=== Session History ==="))
		displayHistory(messages)
		fmt.Println(dim("=== Resuming ==="))
	}

	var isNewSession = sessionID == ""

	for {
		fmt.Print("> ")
		input, err := readLine()
		if err != nil {
			handleExit(messages, isNewSession)
			return
		}

		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}

		if handleCommand(input, &messages, isNewSession) {
			continue
		}

		if isNewSession && currentSessionID == "" {
			currentSessionID = generateSessionID()
		}

		effectiveInput := resolveAnaphoraInput(input, messages)
		messages = append(messages, Message{Role: "user", Content: input})

		plan := jitEngine.Plan(effectiveInput, auth)
		event := JITEvent{
			Timestamp:       time.Now(),
			Mode:            "interactive",
			SessionID:       currentSessionID,
			Prompt:          input,
			EffectivePrompt: effectiveInput,
			PlannedTier:     int(plan.Tier),
			PlannedTierName: tierName(plan.Tier),
			PatternID:       "",
			ScriptName:      "",
			MatchStrategy:   plan.MatchStrategy,
			MatchConfidence: plan.MatchConfidence,
		}
		if effectiveInput != input {
			event.AnaphoraFrom = input
			event.AnaphoraTo = effectiveInput
		}
		if plan.Pattern != nil {
			event.PatternID = plan.Pattern.ID
		}
		if plan.Script != nil {
			event.ScriptName = plan.Script.Name
		}

		switch plan.Tier {
		case Tier3:
			start := time.Now()
			recorder := NewTraceRecorder(input)
			effects, scriptErr := ExecuteScript(plan.Script, recorder)
			if scriptErr != nil {
				traceLog("[jit] deopt tier 3 → tier 0: %v", scriptErr)
				jitEngine.RecordScriptFailure(plan.Script)
				var specContext string
				specContext = buildScriptDeoptContext(plan.Script, effects, scriptErr)
				event.ScriptDeopt = true
				event.InjectedContext = "deopt"
				event.InjectedPreview = specContext
				stats, err := handleResponse(&messages, auth, effectiveInput, plan.Preamble, &specContext, event.ToMeta())
				if err != nil {
					event.Error = err.Error()
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
				} else {
					event.APICalls = stats.APICalls
					event.LLMToolCalls = stats.LLMToolCalls
					event.InputTokens = stats.InputTokens
					event.OutputTokens = stats.OutputTokens
					event.ElapsedMs = stats.ElapsedMs
				}
			} else {
				fmt.Println(dim(fmt.Sprintf("script: 0 API calls, 0 tokens")))
				event.ElapsedMs = float64(time.Since(start).Milliseconds())
				if err := onlineTrainIntentFromTurn(input, true); err != nil && traceJIT {
					traceLog("[jit] online intent train error: %v", err)
				}
			}
			appendJITEvent(event)

		case Tier2:
			var specContext string
			if plan.NeedContext != "" {
				specContext = plan.NeedContext
				event.UsedNeedContext = true
				event.ResolvedNeeds = len(plan.ResolvedNeeds)
				event.InjectedContext = "needctx"
				event.InjectedPreview = specContext
				traceLog("[jit] tier 2: using %d resolved needs", len(plan.ResolvedNeeds))
			} else {
				recorder := NewTraceRecorder(effectiveInput)
				specResults := Speculate(effectiveInput, plan.Pattern, recorder)
				event.SpeculatedOps = len(specResults)
				specContext = PackSpecResults(specResults)
				if event.SpeculatedOps > 0 {
					event.InjectedContext = "spec"
					event.InjectedPreview = specContext
				}
			}

			stats, err := handleResponse(&messages, auth, effectiveInput, plan.Preamble, &specContext, event.ToMeta())
			if err != nil {
				event.Error = err.Error()
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			} else {
				event.APICalls = stats.APICalls
				event.LLMToolCalls = stats.LLMToolCalls
				event.InputTokens = stats.InputTokens
				event.OutputTokens = stats.OutputTokens
				event.ElapsedMs = stats.ElapsedMs
				if stats.LLMToolCalls > 0 {
					jitEngine.RecordSpecSuccess(plan.Pattern)
				} else {
					traceLog("[jit] tier 2: no tool calls; skipping speculative success learning")
				}
			}
			appendJITEvent(event)

		default:
			stats, err := handleResponse(&messages, auth, effectiveInput, plan.Preamble, nil, event.ToMeta())
			if err != nil {
				event.Error = err.Error()
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			} else {
				event.APICalls = stats.APICalls
				event.LLMToolCalls = stats.LLMToolCalls
				event.InputTokens = stats.InputTokens
				event.OutputTokens = stats.OutputTokens
				event.ElapsedMs = stats.ElapsedMs
			}
			appendJITEvent(event)
		}

		if currentSessionID != "" {
			if err := updateSession(currentSessionID, messages); err != nil {
				fmt.Fprintf(os.Stderr, "error updating session: %v\n", err)
			} else {
				isNewSession = false
			}
		}
	}
}

func handleCommand(input string, messages *[]Message, isNewSession bool) bool {
	if strings.HasPrefix(input, "/permissions") {
		rest := strings.TrimSpace(strings.TrimPrefix(input, "/permissions"))
		HandlePermissionsCommand(rest)
		return true
	}

	if strings.HasPrefix(input, "/mode") {
		fields := strings.Fields(input)
		if len(fields) == 1 {
			fmt.Println("Mode:", string(currentMode))
			return true
		}
		if len(fields) != 2 {
			fmt.Println("Usage: /mode <build|plan>")
			return true
		}
		switch strings.ToLower(fields[1]) {
		case "build":
			currentMode = ModeBuild
			fmt.Println("Switched mode to build")
		case "plan":
			currentMode = ModePlan
			fmt.Println("Switched mode to plan")
		default:
			fmt.Println("Unknown mode:", fields[1], "(expected build or plan)")
		}
		return true
	}

	switch input {
	case "/save":
		if len(*messages) > 0 {
			var err error
			savedID := currentSessionID
			if currentSessionID == "" {
				savedID, err = saveSessionWithID(*messages)
			} else {
				err = updateSession(currentSessionID, *messages)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "error saving session: %v\n", err)
			} else {
				fmt.Printf("Session saved: %s\n", savedID)
			}
		}
		return true
	case "/sessions", "/ls":
		listSessions()
		return true
	case "/help":
		fmt.Println("  /save           Save session\n  /sessions       List sessions\n  /todos          Show current todos\n  /jit            Show JIT stats\n  /mode           Show or set mode (/mode build|plan)\n  /build          Shortcut for build mode\n  /plan           Shortcut for plan mode\n  /permissions    Show/set permissions (/permissions [cat] [allow|deny|ask])\n  /quit           Exit")
		return true
	case "/build":
		currentMode = ModeBuild
		fmt.Println("Switched mode to build")
		return true
	case "/plan":
		currentMode = ModePlan
		fmt.Println("Switched mode to plan")
		return true
	case "/jit":
		fmt.Println(dim(jitEngine.Stats()))
		return true
	case "/todos":
		displayTodos()
		return true
	case "/quit":
		handleExit(*messages, isNewSession)
		os.Exit(0)
	}
	return false
}

// handleResponse is the core LLM interaction loop.
// preamble is injected into the system prompt (Tier 1+).

func readLine() (string, error) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return "", fmt.Errorf("failed to set raw mode: %v", err)
	}
	defer term.Restore(fd, oldState)

	var input strings.Builder
	buf := make([]byte, 1)

	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return "", err
		}
		if n == 0 {
			continue
		}
		char := buf[0]
		switch char {
		case '\n', '\r':
			fmt.Print("\r\n")
			return input.String(), nil
		case 3:
			fmt.Print("\r\n")
			return "", fmt.Errorf("interrupted")
		case 27: // ESC
			fmt.Print("\r\n")
			return "", fmt.Errorf("interrupted")
		case 4:
			if input.Len() == 0 {
				fmt.Print("\r\n")
				return "", fmt.Errorf("eof")
			}
		case 5: // Ctrl+E
			if lastEditedFile != "" {
				current := input.String()
				fmt.Print("\r\n")
				term.Restore(fd, oldState)
				editor := os.Getenv("EDITOR")
				if editor == "" {
					editor = "vim"
				}
				cmd := exec.Command(editor, lastEditedFile)
				cmd.Stdin = os.Stdin
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				cmd.Run()
				term.MakeRaw(fd)
				fmt.Print("> " + current)
			}
		case 15: // Ctrl+O
			if lastBashOutput != "" {
				current := input.String()
				fmt.Print("\r\n")
				term.Restore(fd, oldState)
				fmt.Print("\033[2m")
				fmt.Print(lastBashOutput)
				fmt.Print("\033[0m")
				if !strings.HasSuffix(lastBashOutput, "\n") {
					fmt.Println()
				}
				term.MakeRaw(fd)
				fmt.Print("> " + current)
			}
		case 127, 8:
			if input.Len() > 0 {
				str := input.String()
				input.Reset()
				input.WriteString(str[:len(str)-1])
				fmt.Print("\b \b")
			}
		default:
			if char >= 32 {
				input.WriteByte(char)
				fmt.Print(string(char))
			}
		}
	}
}

// interruptWatcher coordinates pause/resume so the permission prompt can
// safely read from stdin without racing the watcher goroutine.
var iwMu sync.Mutex
var iwPauseCh chan struct{} // non-nil when a watcher is active; send to request pause
var iwResumeCh chan struct{}
var iwPausedCh chan struct{}

// pauseInterruptWatcher tells the watcher to stop reading stdin and restore
// the terminal, then blocks until it has done so.
func pauseInterruptWatcher() {
	iwMu.Lock()
	p := iwPauseCh
	iwMu.Unlock()
	if p == nil {
		return
	}
	p <- struct{}{}
	<-iwPausedCh // wait for watcher to acknowledge pause
}

// resumeInterruptWatcher unblocks the paused watcher so it re-enters raw mode.
func resumeInterruptWatcher() {
	iwMu.Lock()
	r := iwResumeCh
	iwMu.Unlock()
	if r == nil {
		return
	}
	r <- struct{}{}
}

func startEscInterruptWatcher(onInterrupt func()) func() {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return func() {}
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return func() {}
	}

	// Re-enable OPOST for output processing, and disable VDISCARD so
	// Ctrl+O (byte 15) passes through to the application instead of being
	// swallowed by the macOS tty driver.
	patchTermios := func() {
		if termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA); err == nil {
			termios.Oflag |= unix.OPOST
			termios.Cc[unix.VDISCARD] = 0
			unix.IoctlSetTermios(fd, unix.TIOCSETA, termios)
		}
	}
	patchTermios()

	pauseCh := make(chan struct{}, 1)
	resumeCh := make(chan struct{}, 1)
	pausedCh := make(chan struct{}, 1)
	iwMu.Lock()
	iwPauseCh = pauseCh
	iwResumeCh = resumeCh
	iwPausedCh = pausedCh
	iwMu.Unlock()

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer term.Restore(fd, oldState)
		defer func() {
			iwMu.Lock()
			iwPauseCh = nil
			iwResumeCh = nil
			iwPausedCh = nil
			iwMu.Unlock()
		}()
		var b [1]byte
		for {
			select {
			case <-stop:
				return
			case <-pauseCh:
				// Restore terminal so the permission prompt can use stdin.
				term.Restore(fd, oldState)
				pausedCh <- struct{}{}
				// Block until resumed or stopped.
				select {
				case <-stop:
					return
				case <-resumeCh:
				}
				// Re-enter raw mode.
				if newState, err := term.MakeRaw(fd); err == nil {
					oldState = newState
				}
				patchTermios()
				continue
			default:
			}

			var readfds unix.FdSet
			fdSet(fd, &readfds)
			tv := unix.Timeval{Sec: 0, Usec: 100 * 1000} // 100ms poll
			n, err := unix.Select(fd+1, &readfds, nil, nil, &tv)
			if err != nil || n <= 0 || !fdIsSet(fd, &readfds) {
				continue
			}
			if _, err := unix.Read(fd, b[:]); err != nil {
				continue
			}
			if b[0] == 15 { // Ctrl+O: toggle live output
				showLiveOutput.Store(!showLiveOutput.Load())
				continue
			}
			if b[0] == 27 || b[0] == 3 { // ESC or Ctrl+C
				onInterrupt()
				return
			}
		}
	}()

	return func() {
		close(stop)
		<-done // wait for goroutine to restore terminal state
	}
}

func fdSet(fd int, set *unix.FdSet) {
	set.Bits[fd/64] |= 1 << (uint(fd) % 64)
}

func fdIsSet(fd int, set *unix.FdSet) bool {
	return set.Bits[fd/64]&(1<<(uint(fd)%64)) != 0
}

func handleExit(messages []Message, isNewSession bool) {
	if len(messages) == 0 {
		return
	}
	if currentSessionID == "" {
		if savedID, err := saveSessionWithID(messages); err == nil {
			currentSessionID = savedID
		}
	} else {
		updateSession(currentSessionID, messages)
	}
	fmt.Println()
}

func startSpinner(statusFn func() string) func() {
	spinner := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	done := make(chan struct{})
	finished := make(chan struct{})
	i := 0
	prevLines := 0 // number of output lines drawn on screen last tick
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				// Clear output lines + spinner line
				if prevLines > 0 {
					fmt.Printf("\033[%dA\033[J", prevLines)
				} else {
					fmt.Print("\r\033[K")
				}
				return
			default:
				suffix := ""
				if statusFn != nil {
					suffix = statusFn()
				}

				// Erase previous frame (output lines + spinner line).
				// Cursor is at end of the spinner line from last tick.
				if prevLines > 0 {
					// Move up past output lines, then clear everything below.
					fmt.Printf("\r\033[%dA\033[J", prevLines)
				} else {
					fmt.Print("\r\033[K")
				}
				prevLines = 0

				// Draw live output tail if toggled on.
				if showLiveOutput.Load() {
					snap := getActiveCmdSnapshot(15)
					if snap != "" {
						lines := strings.Split(snap, "\n")
						for _, line := range lines {
							fmt.Printf("  \033[2m%s\033[0m\n", line)
						}
						prevLines = len(lines)
					}
				}

				// Draw spinner on the bottom line.
				fmt.Printf("\033[1;32m%s\033[0m Thinking...%s", spinner[i%len(spinner)], suffix)
				i++
				time.Sleep(100 * time.Millisecond)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func displayHistory(messages []Message) {
	for i, msg := range messages {
		if i >= 10 {
			fmt.Println(dim("... (" + fmt.Sprint(len(messages)-10) + " earlier messages)"))
			break
		}
		switch msg.Role {
		case "user":
			if str, ok := msg.Content.(string); ok {
				preview := str
				if len(preview) > 80 {
					preview = preview[:77] + "..."
				}
				fmt.Println(dim("User: " + preview))
			}
		case "assistant":
			fmt.Println(dim("Assistant: (responded)"))
		}
	}
}

func dim(s string) string {
	lines := strings.Split(s, "\n")
	var b strings.Builder
	for i, line := range lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		if line != "" {
			b.WriteString("  \033[2m")
			b.WriteString(line)
			b.WriteString("\033[0m")
		}
	}
	return b.String()
}

type Session struct {
	ID                 string     `json:"id"`
	Created            time.Time  `json:"created"`
	Updated            time.Time  `json:"updated"`
	CWD                string     `json:"cwd,omitempty"`
	Messages           []Message  `json:"messages"`
	CompactionBoundary int        `json:"compaction_boundary,omitempty"`
	CompactionSummary  string     `json:"compaction_summary,omitempty"`
	Todos              []TodoItem `json:"todos,omitempty"`
}

func getSessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	sessionDir := filepath.Join(home, ".oc", "sessions")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		return "", err
	}

	return sessionDir, nil
}

func generateSessionID() string {
	return strconv.FormatInt(time.Now().Unix(), 10)
}

func saveSessionWithID(messages []Message) (string, error) {
	messages = sanitizeMessagesForSession(messages)
	if len(messages) == 0 {
		return "", nil
	}
	if !shouldPersistNewSession(messages) {
		return "", nil
	}

	sessionDir, err := getSessionDir()
	if err != nil {
		return "", err
	}

	sessionID := generateSessionID()
	cwd, _ := os.Getwd()
	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	session := Session{
		ID:                 sessionID,
		Created:            time.Now(),
		Updated:            time.Now(),
		CWD:                cwd,
		Messages:           messages,
		CompactionBoundary: currentCompactionBoundary,
		CompactionSummary:  currentCompactionSummary,
		Todos:              cloneTodos(currentTodos),
	}

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return "", err
	}

	sessionFile := filepath.Join(sessionDir, sessionID+".json")
	if err := os.WriteFile(sessionFile, data, 0o644); err != nil {
		return "", err
	}

	return sessionID, nil
}

func updateSession(sessionID string, messages []Message) error {
	messages = sanitizeMessagesForSession(messages)
	if len(messages) == 0 {
		return nil
	}

	sessionDir, err := getSessionDir()
	if err != nil {
		return err
	}

	sessionFile := filepath.Join(sessionDir, sessionID+".json")

	var session Session
	if data, err := os.ReadFile(sessionFile); err == nil {
		_ = json.Unmarshal(data, &session)
	}
	if session.ID == "" {
		session.ID = sessionID
	}
	if session.Created.IsZero() {
		session.Created = time.Now()
	}
	if strings.TrimSpace(session.CWD) == "" {
		cwd, _ := os.Getwd()
		if abs, err := filepath.Abs(cwd); err == nil {
			cwd = abs
		}
		session.CWD = cwd
	}

	session.Messages = messages
	session.CompactionBoundary = currentCompactionBoundary
	session.CompactionSummary = currentCompactionSummary
	session.Todos = cloneTodos(currentTodos)
	session.Updated = time.Now()

	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(sessionFile, data, 0o644)
}

func loadSession(sessionID string) ([]Message, error) {
	sessionDir, err := getSessionDir()
	if err != nil {
		return nil, err
	}

	sessionFile := filepath.Join(sessionDir, sessionID+".json")
	data, err := os.ReadFile(sessionFile)
	if err != nil {
		return nil, err
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	if strings.TrimSpace(session.CompactionSummary) != "" {
		currentCompactionBoundary = session.CompactionBoundary
		currentCompactionSummary = session.CompactionSummary
	} else {
		currentCompactionBoundary = -1
		currentCompactionSummary = ""
	}
	currentTodos = cloneTodos(session.Todos)

	fmt.Println(dim("resumed session: " + sessionID))
	return session.Messages, nil
}

func listSessions() error {
	sessionDir, err := getSessionDir()
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return err
	}

	var sessions []Session
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			sessionFile := filepath.Join(sessionDir, entry.Name())
			data, err := os.ReadFile(sessionFile)
			if err != nil {
				continue
			}

			var session Session
			if err := json.Unmarshal(data, &session); err != nil {
				continue
			}
			sessions = append(sessions, session)
		}
	}

	if len(sessions) == 0 {
		fmt.Println("No saved sessions found.")
		return nil
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].Updated.After(sessions[j].Updated)
	})

	fmt.Println("Saved sessions:")
	for _, session := range sessions {
		messageCount := len(session.Messages)
		var preview string

		for _, msg := range session.Messages {
			if msg.Role == "user" {
				if str, ok := msg.Content.(string); ok {
					preview = normalizeUserPreview(str)
					if len(preview) > 50 {
						preview = preview[:47] + "..."
					}
					break
				}
			}
		}

		if preview == "" {
			preview = "(no user messages)"
		}

		fmt.Printf("  %s  %s  %d messages  %s\n",
			session.ID,
			session.Updated.Format("2006-01-02 15:04"),
			messageCount,
			preview)
	}

	return nil
}

func findLatestSessionForCWD(cwd string) (string, error) {
	sessionDir, err := getSessionDir()
	if err != nil {
		return "", err
	}

	if abs, err := filepath.Abs(cwd); err == nil {
		cwd = abs
	}
	cwd = filepath.Clean(strings.TrimSpace(cwd))
	if cwd == "" {
		return "", fmt.Errorf("current working directory is empty")
	}

	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return "", err
	}

	var best Session
	found := false
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		sessionFile := filepath.Join(sessionDir, entry.Name())
		data, err := os.ReadFile(sessionFile)
		if err != nil {
			continue
		}
		var session Session
		if err := json.Unmarshal(data, &session); err != nil {
			continue
		}
		if strings.TrimSpace(session.CWD) == "" {
			continue
		}
		sessCWD := filepath.Clean(session.CWD)
		if sessCWD != cwd {
			continue
		}
		if !found || session.Updated.After(best.Updated) {
			best = session
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("no saved sessions found for cwd: %s", cwd)
	}
	return best.ID, nil
}

func normalizeUserPreview(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	lower := strings.ToLower(s)
	if idx := strings.LastIndex(lower, "user request:"); idx >= 0 {
		v := strings.TrimSpace(s[idx+len("user request:"):])
		if v != "" {
			return v
		}
	}
	if strings.HasPrefix(s, "IMPORTANT: The following context has already been gathered for you.") {
		return "(internal context prompt)"
	}
	return s
}

func sanitizeMessagesForSession(messages []Message) []Message {
	out := make([]Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "user" {
			if str, ok := msg.Content.(string); ok {
				clean := normalizeUserPreview(str)
				clean = strings.TrimSpace(clean)
				if clean == "" || clean == "(internal context prompt)" {
					continue
				}
				msg.Content = clean
			}
		}
		out = append(out, msg)
	}
	return out
}

func shouldPersistNewSession(messages []Message) bool {
	hasUser := false
	hasAssistant := false
	for _, msg := range messages {
		if msg.Role == "user" {
			if str, ok := msg.Content.(string); ok && strings.TrimSpace(str) != "" {
				hasUser = true
			}
		}
		if msg.Role == "assistant" {
			hasAssistant = true
		}
	}
	return hasUser && hasAssistant
}

var renderer *glamour.TermRenderer

func init() {
	r, err := glamour.NewTermRenderer(
		glamour.WithStandardStyle("dark"),
		glamour.WithWordWrap(0),
	)
	if err != nil {
		return
	}
	renderer = r
}

func renderMarkdown(text string) string {
	if renderer == nil {
		return text
	}
	out, err := renderer.Render(text)
	if err != nil {
		return text
	}
	return strings.Trim(out, "\n")
}

func termWidth() int {
	w, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || w <= 0 {
		return 80
	}
	return w
}

func extToLang(path string) string {
	ext := filepath.Ext(path)
	if lang := langFromExt(ext); lang != "" {
		return lang
	}
	e := strings.TrimPrefix(ext, ".")
	switch e {
	case "yml", "yaml":
		return "yaml"
	case "md":
		return "markdown"
	case "":
		return ""
	default:
		return e
	}
}

func renderDiff(oldContent, newContent, path string) string {
	diff := difflib.UnifiedDiff{
		A:        difflib.SplitLines(oldContent),
		B:        difflib.SplitLines(newContent),
		FromFile: path,
		ToFile:   path,
		Context:  3,
	}
	text, err := difflib.GetUnifiedDiffString(diff)
	if err != nil || text == "" {
		return dim("(no changes)")
	}

	width := termWidth()
	bar := strings.Repeat("─", width-4)

	var b strings.Builder

	b.WriteString("  \033[2m")
	b.WriteString(bar)
	b.WriteString("\033[0m\n")

	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}

		if strings.HasPrefix(line, "+") {
			b.WriteString("  \033[32;48;5;22m + ")
			b.WriteString(line[1:])
			b.WriteString(" \033[0m\n")
		} else if strings.HasPrefix(line, "-") {
			b.WriteString("  \033[31;48;5;52m - ")
			b.WriteString(line[1:])
			b.WriteString(" \033[0m\n")
		} else if strings.HasPrefix(line, "@@") {
			b.WriteString("  \033[36;2m")
			b.WriteString(line)
			b.WriteString("\033[0m\n")
		} else {
			b.WriteString("  \033[2m   ")
			if len(line) > 1 {
				b.WriteString(line[1:])
			}
			b.WriteString("\033[0m\n")
		}
	}

	b.WriteString("  \033[2m")
	b.WriteString(bar)
	b.WriteString("\033[0m")

	return b.String()
}

func renderNewFile(content, path string) string {
	width := termWidth()
	bar := strings.Repeat("─", width-4)

	lang := extToLang(path)
	var md string
	if lang != "" {
		md = fmt.Sprintf("```%s\n%s\n```", lang, content)
	} else {
		md = fmt.Sprintf("```\n%s\n```", content)
	}
	rendered := renderMarkdown(md)

	var b strings.Builder

	b.WriteString("  \033[2m")
	b.WriteString(bar)
	b.WriteString("\033[0m\n")

	for _, line := range strings.Split(rendered, "\n") {
		b.WriteString("  \033[32;48;5;22m + ")
		b.WriteString(line)
		b.WriteString(" \033[0m\n")
	}

	b.WriteString("  \033[2m")
	b.WriteString(bar)
	b.WriteString("\033[0m")

	return b.String()
}

// ocBaseDir returns the .oc directory (at git root or cwd).
func ocBaseDir() (string, error) {
	dir, err := getTraceDir()
	if err != nil {
		return "", err
	}
	return filepath.Dir(dir), nil
}

// langFromExt returns a language identifier for the given file extension.
func langFromExt(ext string) string {
	switch ext {
	case ".go":
		return "go"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".js", ".ts", ".jsx", ".tsx":
		return "node"
	case ".rb":
		return "ruby"
	case ".java":
		return "java"
	case ".c", ".h":
		return "c"
	case ".cpp", ".cc", ".cxx", ".hpp":
		return "cpp"
	case ".cs":
		return "csharp"
	case ".swift":
		return "swift"
	case ".kt", ".kts":
		return "kotlin"
	case ".lua":
		return "lua"
	case ".sh", ".bash":
		return "shell"
	default:
		return ""
	}
}

