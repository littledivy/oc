package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/term"
)

// Permission categories modeled after Deno's permission system.
// Each category can be Allow, Deny, or Ask (prompt on first use).
// Categories may also carry granular scopes (allowed paths, hosts, commands).
//
// The code tool (Deno) does not have its own permission category. Its sandbox
// is derived from the other four categories via DenoPermFlags(). If read is
// scoped to ./src, the code tool can only read ./src. If bash is denied, the
// code tool cannot spawn subprocesses. This avoids a redundant gate -- the
// Deno permission flags *are* the enforcement.
type PermState int

const (
	PermAsk   PermState = iota // prompt user on first use (default)
	PermAllow                  // always allow (or allow if scope matches)
	PermDeny                   // always deny
)

func (p PermState) String() string {
	switch p {
	case PermAllow:
		return "allow"
	case PermDeny:
		return "deny"
	default:
		return "ask"
	}
}

func parsePermState(s string) PermState {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow", "a":
		return PermAllow
	case "deny", "d":
		return PermDeny
	default:
		return PermAsk
	}
}

// PermCategory is a permission domain, analogous to Deno's --allow-* flags.
type PermCategory string

const (
	PermRead  PermCategory = "read"  // read_file, read_files, list_files, glob, grep, find_symbol
	PermWrite PermCategory = "write" // write_file, write_files, edit_file, regex_edit
	PermBash  PermCategory = "bash"  // bash tool execution
	PermNet   PermCategory = "net"   // webfetch
)

var allPermCategories = []PermCategory{PermRead, PermWrite, PermBash, PermNet}

// PermRule holds the state and optional granular scope for a category.
// Scopes work like Deno: empty means "all" when state is Allow,
// non-empty means "only these" (path prefixes for read/write, hosts for net,
// command prefixes for bash).
type PermRule struct {
	State  PermState
	Scopes []string // empty = unrestricted within state
}

// PermissionSet holds the state for each category.
type PermissionSet struct {
	mu       sync.RWMutex
	rules    map[PermCategory]*PermRule
	session  map[PermCategory]*PermRule // session-level overrides from interactive prompts
	filepath string                     // where to persist
}

// permRuleConfig is the JSON representation of a single rule.
type permRuleConfig struct {
	State  string   `json:"state"`
	Scopes []string `json:"scopes,omitempty"`
}

// permissionConfig is the JSON representation.
type permissionConfig struct {
	Read  permRuleConfig `json:"read"`
	Write permRuleConfig `json:"write"`
	Bash  permRuleConfig `json:"bash"`
	Net   permRuleConfig `json:"net"`
}

var permissions *PermissionSet

func initPermissions() {
	permissions = newPermissionSet()
	permissions.Load()

	// Environment override: OC_PERMISSIONS="read:allow,write:ask,bash:deny,net:deny"
	// Granular: OC_PERMISSIONS="read:allow=./src:./lib,write:allow=.,net:deny,bash:allow=go:cargo"
	if env := os.Getenv("OC_PERMISSIONS"); env != "" {
		for _, part := range strings.Split(env, ",") {
			part = strings.TrimSpace(part)
			eqIdx := strings.Index(part, ":")
			if eqIdx < 0 {
				continue
			}
			cat := PermCategory(part[:eqIdx])
			rest := part[eqIdx+1:]
			// rest is "allow=./src:./lib" or just "allow"
			if scopeIdx := strings.Index(rest, "="); scopeIdx >= 0 {
				state := parsePermState(rest[:scopeIdx])
				scopes := strings.Split(rest[scopeIdx+1:], ":")
				permissions.SetWithScopes(cat, state, scopes)
			} else {
				permissions.Set(cat, parsePermState(rest))
			}
		}
	}
}

func newPermissionSet() *PermissionSet {
	ps := &PermissionSet{
		rules:   make(map[PermCategory]*PermRule),
		session: make(map[PermCategory]*PermRule),
	}
	// Defaults: read is allowed, everything else prompts.
	ps.rules[PermRead] = &PermRule{State: PermAllow}
	ps.rules[PermWrite] = &PermRule{State: PermAsk}
	ps.rules[PermBash] = &PermRule{State: PermAsk}
	ps.rules[PermNet] = &PermRule{State: PermAsk}
	return ps
}

func (ps *PermissionSet) Set(cat PermCategory, state PermState) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.rules[cat] = &PermRule{State: state}
}

func (ps *PermissionSet) SetWithScopes(cat PermCategory, state PermState, scopes []string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	cleaned := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s != "" {
			cleaned = append(cleaned, s)
		}
	}
	ps.rules[cat] = &PermRule{State: state, Scopes: cleaned}
}

func (ps *PermissionSet) getRule(cat PermCategory) *PermRule {
	ps.mu.RLock()
	defer ps.mu.RUnlock()
	if r, ok := ps.session[cat]; ok {
		return r
	}
	if r, ok := ps.rules[cat]; ok {
		return r
	}
	return &PermRule{State: PermAsk}
}

func (ps *PermissionSet) Get(cat PermCategory) PermState {
	return ps.getRule(cat).State
}

// SetSession records an interactive allow/deny decision for the rest of this session.
func (ps *PermissionSet) SetSession(cat PermCategory, state PermState) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.session[cat] = &PermRule{State: state}
}

// Check returns nil if allowed, or a ToolResult error if denied.
// For PermAsk, it prompts the user interactively.
// resource is the specific path/host/command being accessed for scope checking.
func (ps *PermissionSet) Check(cat PermCategory, description string, resource string) *ToolResult {
	rule := ps.getRule(cat)
	switch rule.State {
	case PermAllow:
		if len(rule.Scopes) > 0 && resource != "" {
			if !matchesScope(cat, resource, rule.Scopes) {
				// Allowed with scopes, but this resource is outside scope -- prompt.
				return ps.prompt(cat, description+" (outside allowed scope)")
			}
		}
		return nil
	case PermDeny:
		return &ToolResult{
			Content: fmt.Sprintf("Permission denied: %s. The %q permission is set to deny. Use /permissions to change.", description, cat),
			IsError: true,
		}
	default: // PermAsk
		return ps.prompt(cat, description)
	}
}

// matchesScope checks if a resource matches any of the allowed scopes.
func matchesScope(cat PermCategory, resource string, scopes []string) bool {
	switch cat {
	case PermRead, PermWrite:
		return matchesPathScope(resource, scopes)
	case PermNet:
		return matchesHostScope(resource, scopes)
	case PermBash:
		return matchesCmdScope(resource, scopes)
	default:
		return true
	}
}

// matchesPathScope checks if a file path falls under any allowed path prefix.
func matchesPathScope(path string, scopes []string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	for _, scope := range scopes {
		scopeAbs, err := filepath.Abs(scope)
		if err != nil {
			scopeAbs = scope
		}
		if abs == scopeAbs || strings.HasPrefix(abs, scopeAbs+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// matchesHostScope checks if a URL's host matches any allowed host pattern.
func matchesHostScope(rawURL string, scopes []string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, scope := range scopes {
		scope = strings.ToLower(strings.TrimSpace(scope))
		if host == scope {
			return true
		}
		// Allow wildcard subdomains: *.example.com matches sub.example.com
		if strings.HasPrefix(scope, "*.") {
			suffix := scope[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) {
				return true
			}
		}
	}
	return false
}

// matchesCmdScope checks if a bash command starts with any allowed command prefix.
func matchesCmdScope(cmd string, scopes []string) bool {
	trimmed := strings.TrimSpace(cmd)
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if trimmed == scope || strings.HasPrefix(trimmed, scope+" ") || strings.HasPrefix(trimmed, scope+"\n") {
			return true
		}
	}
	return false
}

// prompt asks the user for permission via the terminal.
// It pauses the escape-interrupt watcher first so it can safely read stdin.
func (ps *PermissionSet) prompt(cat PermCategory, description string) *ToolResult {
	// Pause the interrupt watcher so we can read from stdin without racing.
	pauseInterruptWatcher()
	defer resumeInterruptWatcher()

	fmt.Printf("\n  \033[1;33m? Permission request: %s\033[0m\n", cat)
	fmt.Printf("    %s\n", description)
	fmt.Printf("    [\033[1;32my\033[0m]es  [\033[1;31mn\033[0m]o  [\033[1;32mA\033[0m]llow always  [\033[1;31mD\033[0m]eny always: ")

	response := readPermKey()

	// Clear the permission prompt lines (blank+title, description, choices).
	clearPrompt := func() {
		fmt.Print("\r\033[3A\033[J")
	}

	switch response {
	case "y", "yes":
		clearPrompt()
		return nil
	case "n", "no":
		fmt.Print("\r\n")
		return &ToolResult{
			Content: fmt.Sprintf("Permission denied by user: %s (%s)", cat, description),
			IsError: true,
		}
	case "a", "allow":
		ps.SetSession(cat, PermAllow)
		clearPrompt()
		return nil
	case "d", "deny":
		ps.SetSession(cat, PermDeny)
		fmt.Print("\r\n")
		fmt.Printf("    \033[31m%s permission denied for this session\033[0m\n", cat)
		return &ToolResult{
			Content: fmt.Sprintf("Permission denied by user for session: %s (%s)", cat, description),
			IsError: true,
		}
	default:
		clearPrompt()
		return nil
	}
}

// categorize maps a tool call to (category, description, resource).
// resource is the specific path/host/command for granular scope checking.
// The code tool and run_skill are not categorized here -- their permissions
// are enforced by the Deno sandbox via DenoPermFlags().
func categorize(name string, input json.RawMessage) (PermCategory, string, string) {
	switch name {
	case "read_file":
		path := extractToolFilePath(name, input)
		return PermRead, fmt.Sprintf("Read file: %s", path), path
	case "read_files":
		paths := parseReadFilesArgs(input)
		return PermRead, fmt.Sprintf("Read %d files: %s", len(paths), strings.Join(paths, ", ")), strings.Join(paths, ",")
	case "list_files":
		path := extractToolFilePath(name, input)
		if path == "" {
			path = "."
		}
		return PermRead, fmt.Sprintf("List directory: %s", path), path
	case "glob":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(input, &a)
		p := orDefault(a.Path, ".")
		return PermRead, fmt.Sprintf("Glob search: %s in %s", a.Pattern, p), p
	case "grep":
		var a struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		_ = json.Unmarshal(input, &a)
		p := orDefault(a.Path, ".")
		return PermRead, fmt.Sprintf("Grep: %q in %s", a.Pattern, p), p
	case "find_symbol":
		var a struct {
			Symbol string `json:"symbol"`
			Path   string `json:"path"`
		}
		_ = json.Unmarshal(input, &a)
		p := orDefault(a.Path, ".")
		return PermRead, fmt.Sprintf("Find symbol: %s", a.Symbol), p

	case "write_file":
		path := extractToolFilePath(name, input)
		return PermWrite, fmt.Sprintf("Write file: %s", path), path
	case "write_files":
		items := parseWriteFilesArgs(input)
		paths := make([]string, len(items))
		for i, f := range items {
			paths[i] = f.Path
		}
		return PermWrite, fmt.Sprintf("Write %d files: %s", len(items), strings.Join(paths, ", ")), strings.Join(paths, ",")
	case "edit_file":
		path := extractToolFilePath(name, input)
		return PermWrite, fmt.Sprintf("Edit file: %s", path), path
	case "regex_edit":
		var a struct {
			Glob string `json:"glob"`
		}
		_ = json.Unmarshal(input, &a)
		return PermWrite, fmt.Sprintf("Regex edit across: %s", a.Glob), "."

	case "bash":
		cmd := extractBashCommand(input)
		return PermBash, fmt.Sprintf("Run command: %s", truncateStr(cmd, 80)), cmd

	case "webfetch":
		var a struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(input, &a)
		return PermNet, fmt.Sprintf("Fetch URL: %s", a.URL), a.URL

	default:
		// code, run_skill, todowrite, etc. -- no oc-level gate needed.
		// code/run_skill are sandboxed by Deno flags from DenoPermFlags().
		return "", "", ""
	}
}

// CheckToolPermission checks whether a tool call is allowed.
// Returns nil if allowed, or a ToolResult to return to the LLM if denied.
func CheckToolPermission(name string, input json.RawMessage) *ToolResult {
	if permissions == nil {
		return nil
	}

	cat, desc, resource := categorize(name, input)
	if cat == "" {
		return nil
	}

	// For multi-path tools (read_files, write_files), check each path individually.
	if (name == "read_files" || name == "write_files") && resource != "" {
		paths := strings.Split(resource, ",")
		rule := permissions.getRule(cat)
		if rule.State == PermAllow && len(rule.Scopes) > 0 {
			for _, p := range paths {
				p = strings.TrimSpace(p)
				if p != "" && !matchesPathScope(p, rule.Scopes) {
					return permissions.prompt(cat, desc+" ("+p+" outside allowed scope)")
				}
			}
			return nil
		}
	}

	return permissions.Check(cat, desc, resource)
}

// --- Deno flag generation ---

// DenoPermFlags returns Deno CLI permission flags derived from the current
// oc permission state. This ensures the code tool sandbox mirrors what oc allows.
// There is no separate "code" permission -- the code tool inherits read/write/bash/net.
func DenoPermFlags() []string {
	if permissions == nil {
		// No permission system -- fall back to scoped defaults.
		cwd, _ := os.Getwd()
		return []string{
			"--allow-read=" + cwd + ",/tmp",
			"--allow-write=" + cwd + ",/tmp",
			"--allow-run=find,grep,bash",
			"--allow-env=OC_ASK_CALLBACK",
		}
	}

	var flags []string
	cwd, _ := os.Getwd()

	// --allow-read
	readRule := permissions.getRule(PermRead)
	switch readRule.State {
	case PermAllow:
		if len(readRule.Scopes) > 0 {
			flags = append(flags, "--allow-read="+joinAbsPaths(readRule.Scopes)+",/tmp")
		} else {
			flags = append(flags, "--allow-read="+cwd+",/tmp")
		}
	case PermAsk:
		// Conservative: scope to cwd.
		flags = append(flags, "--allow-read="+cwd+",/tmp")
	// PermDeny: no --allow-read at all -- Deno will deny reads.
	}

	// --allow-write
	writeRule := permissions.getRule(PermWrite)
	switch writeRule.State {
	case PermAllow:
		if len(writeRule.Scopes) > 0 {
			flags = append(flags, "--allow-write="+joinAbsPaths(writeRule.Scopes)+",/tmp")
		} else {
			flags = append(flags, "--allow-write="+cwd+",/tmp")
		}
	case PermAsk:
		flags = append(flags, "--allow-write="+cwd+",/tmp")
	// PermDeny: no --allow-write.
	}

	// --allow-run
	bashRule := permissions.getRule(PermBash)
	switch bashRule.State {
	case PermAllow:
		if len(bashRule.Scopes) > 0 {
			// Scoped bash: allow find,grep + scoped commands.
			cmds := dedupeStrings(append([]string{"find", "grep"}, bashRule.Scopes...))
			flags = append(flags, "--allow-run="+strings.Join(cmds, ","))
		} else {
			flags = append(flags, "--allow-run=find,grep,bash")
		}
	case PermAsk:
		// Conservative: only allow find/grep for oc.glob/oc.grep.
		// oc.bash() will be blocked by Deno -- user must allow bash to use it.
		flags = append(flags, "--allow-run=find,grep")
	// PermDeny: no --allow-run at all.
	}

	// --allow-net
	netRule := permissions.getRule(PermNet)
	switch netRule.State {
	case PermAllow:
		if len(netRule.Scopes) > 0 {
			flags = append(flags, "--allow-net="+strings.Join(netRule.Scopes, ","))
		} else {
			flags = append(flags, "--allow-net")
		}
	// PermAsk, PermDeny: no --allow-net.
	}

	// --allow-env (always needed for oc.ask callback)
	flags = append(flags, "--allow-env=OC_ASK_CALLBACK")

	return flags
}

// joinAbsPaths resolves paths to absolute and joins with comma.
func joinAbsPaths(paths []string) string {
	abs := make([]string, 0, len(paths))
	for _, p := range paths {
		a, err := filepath.Abs(p)
		if err != nil {
			a = p
		}
		abs = append(abs, a)
	}
	return strings.Join(abs, ",")
}

func dedupeStrings(ss []string) []string {
	seen := make(map[string]bool, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// --- Persistence ---

func permissionsPath() string {
	base, err := ocBaseDir()
	if err != nil || base == "" {
		return ""
	}
	return filepath.Join(base, "permissions.json")
}

func (ps *PermissionSet) Load() {
	path := permissionsPath()
	if path == "" {
		return
	}
	ps.filepath = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var cfg permissionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return
	}
	loadRule := func(cat PermCategory, rc permRuleConfig) {
		if rc.State == "" {
			return
		}
		ps.rules[cat] = &PermRule{
			State:  parsePermState(rc.State),
			Scopes: rc.Scopes,
		}
	}
	loadRule(PermRead, cfg.Read)
	loadRule(PermWrite, cfg.Write)
	loadRule(PermBash, cfg.Bash)
	loadRule(PermNet, cfg.Net)
}

func (ps *PermissionSet) Save() error {
	if ps.filepath == "" {
		path := permissionsPath()
		if path == "" {
			return fmt.Errorf("no .oc directory found")
		}
		ps.filepath = path
	}
	ps.mu.RLock()
	ruleToConfig := func(cat PermCategory) permRuleConfig {
		r := ps.rules[cat]
		if r == nil {
			return permRuleConfig{State: "ask"}
		}
		return permRuleConfig{State: r.State.String(), Scopes: r.Scopes}
	}
	cfg := permissionConfig{
		Read:  ruleToConfig(PermRead),
		Write: ruleToConfig(PermWrite),
		Bash:  ruleToConfig(PermBash),
		Net:   ruleToConfig(PermNet),
	}
	ps.mu.RUnlock()

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	dir := filepath.Dir(ps.filepath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(ps.filepath, append(data, '\n'), 0o644)
}

// --- CLI ---

// ApplyPermissionFlags processes --allow-* and --deny-* flags and removes them from args.
// Supports granular scopes: --allow-read=./src,./lib --allow-net=example.com --allow-bash=go,cargo
func ApplyPermissionFlags(args []string) []string {
	var remaining []string
	for _, arg := range args {
		if handled := applyPermFlag(arg); !handled {
			remaining = append(remaining, arg)
		}
	}
	return remaining
}

func applyPermFlag(arg string) bool {
	switch {
	case arg == "--allow-all":
		for _, cat := range allPermCategories {
			permissions.Set(cat, PermAllow)
		}
		return true
	case arg == "--deny-all":
		for _, cat := range allPermCategories {
			if cat != PermRead {
				permissions.Set(cat, PermDeny)
			}
		}
		return true
	}

	// --allow-<cat> or --allow-<cat>=<scopes> or --deny-<cat>
	for _, cat := range allPermCategories {
		allowPrefix := "--allow-" + string(cat)
		denyPrefix := "--deny-" + string(cat)
		if arg == allowPrefix {
			permissions.Set(cat, PermAllow)
			return true
		}
		if strings.HasPrefix(arg, allowPrefix+"=") {
			scopes := strings.Split(arg[len(allowPrefix)+1:], ",")
			permissions.SetWithScopes(cat, PermAllow, scopes)
			return true
		}
		if arg == denyPrefix {
			permissions.Set(cat, PermDeny)
			return true
		}
	}
	return false
}

// ShowPermissions displays current permission states.
func ShowPermissions() {
	if permissions == nil {
		fmt.Println("  Permissions not initialized")
		return
	}
	permissions.mu.RLock()
	defer permissions.mu.RUnlock()

	fmt.Println("  Permissions (code tool inherits these via Deno sandbox):")
	for _, cat := range allPermCategories {
		base := permissions.rules[cat]
		if base == nil {
			base = &PermRule{State: PermAsk}
		}
		effective := base
		if s, ok := permissions.session[cat]; ok {
			effective = s
		}
		line := fmt.Sprintf("    %-6s %s", cat, colorPermState(effective.State))
		if len(effective.Scopes) > 0 {
			line += " " + dim("["+strings.Join(effective.Scopes, ", ")+"]")
		}
		if s, ok := permissions.session[cat]; ok && (s.State != base.State || len(s.Scopes) != len(base.Scopes)) {
			line += " " + dim(fmt.Sprintf("(config: %s, session: %s)", base.State, s.State))
		}
		fmt.Println(line)
	}
	fmt.Println()
	fmt.Println("  Set:     /permissions <cat> <allow|deny|ask> [scope,scope,...]")
	fmt.Println("  Persist: /permissions save")
	fmt.Println("  Reset:   /permissions reset")
	fmt.Println("  Flags:   --allow-read=./src --deny-net --allow-bash=go,cargo")
}

// HandlePermissionsCommand handles /permissions [args].
func HandlePermissionsCommand(args string) {
	args = strings.TrimSpace(args)

	if args == "" {
		ShowPermissions()
		return
	}

	if args == "save" {
		if err := permissions.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "  Error saving permissions: %v\n", err)
		} else {
			fmt.Println("  Permissions saved to", permissions.filepath)
		}
		return
	}

	if args == "reset" {
		ps := newPermissionSet()
		permissions.mu.Lock()
		permissions.rules = ps.rules
		permissions.session = make(map[PermCategory]*PermRule)
		permissions.mu.Unlock()
		fmt.Println("  Permissions reset to defaults")
		return
	}

	fields := strings.Fields(args)
	if len(fields) >= 2 {
		cat := PermCategory(fields[0])
		valid := false
		for _, c := range allPermCategories {
			if c == cat {
				valid = true
				break
			}
		}
		if !valid {
			fmt.Printf("  Unknown category: %s (valid: read, write, bash, net)\n", fields[0])
			return
		}
		state := parsePermState(fields[1])

		var scopes []string
		if len(fields) >= 3 {
			scopes = strings.Split(fields[2], ",")
		}

		if len(scopes) > 0 {
			permissions.SetWithScopes(cat, state, scopes)
		} else {
			permissions.Set(cat, state)
		}
		// Clear session override for this category.
		permissions.mu.Lock()
		delete(permissions.session, cat)
		permissions.mu.Unlock()

		msg := fmt.Sprintf("  %s -> %s", cat, colorPermState(state))
		if len(scopes) > 0 {
			msg += " [" + strings.Join(scopes, ", ") + "]"
		}
		fmt.Println(msg)
		return
	}

	fmt.Println("  Usage: /permissions [<cat> <allow|deny|ask> [scope,...]] [save|reset]")
}

func colorPermState(s PermState) string {
	switch s {
	case PermAllow:
		return "\033[32mallow\033[0m"
	case PermDeny:
		return "\033[31mdeny\033[0m"
	default:
		return "\033[33mask\033[0m"
	}
}

// readPermKey reads a single keypress from the terminal in raw mode.
// This avoids the race with startEscInterruptWatcher which also reads stdin.
func readPermKey() string {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		// Fallback for non-terminal (pipe, etc.)
		var s string
		fmt.Scanln(&s)
		return strings.ToLower(strings.TrimSpace(s))
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		var s string
		fmt.Scanln(&s)
		return strings.ToLower(strings.TrimSpace(s))
	}
	defer term.Restore(fd, oldState)

	var buf [1]byte
	for {
		n, err := os.Stdin.Read(buf[:])
		if err != nil || n == 0 {
			return ""
		}
		ch := buf[0]
		// Ignore control chars except the ones we care about
		if ch == 3 || ch == 27 { // Ctrl-C / ESC → treat as deny
			return "n"
		}
		if ch >= 32 && ch < 127 {
			return strings.ToLower(string(ch))
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}
