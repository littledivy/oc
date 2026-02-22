package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPermStateStringRoundtrip(t *testing.T) {
	for _, tc := range []struct {
		state PermState
		str   string
	}{
		{PermAllow, "allow"},
		{PermDeny, "deny"},
		{PermAsk, "ask"},
	} {
		if tc.state.String() != tc.str {
			t.Errorf("PermState(%d).String() = %q, want %q", tc.state, tc.state.String(), tc.str)
		}
		if parsePermState(tc.str) != tc.state {
			t.Errorf("parsePermState(%q) = %d, want %d", tc.str, parsePermState(tc.str), tc.state)
		}
	}
}

func TestCategorize(t *testing.T) {
	tests := []struct {
		tool    string
		input   string
		wantCat PermCategory
	}{
		{"read_file", `{"path":"main.go"}`, PermRead},
		{"read_files", `{"paths":["a.go","b.go"]}`, PermRead},
		{"list_files", `{"path":"."}`, PermRead},
		{"glob", `{"pattern":"**/*.go"}`, PermRead},
		{"grep", `{"pattern":"TODO"}`, PermRead},
		{"find_symbol", `{"symbol":"main"}`, PermRead},
		{"write_file", `{"path":"out.go","content":"x"}`, PermWrite},
		{"write_files", `{"files":[{"path":"a.go","content":"x"}]}`, PermWrite},
		{"edit_file", `{"path":"a.go","old_string":"x","new_string":"y"}`, PermWrite},
		{"regex_edit", `{"pattern":"x","replacement":"y","glob":"*.go"}`, PermWrite},
		{"bash", `{"command":"ls -la"}`, PermBash},
		{"webfetch", `{"url":"https://example.com"}`, PermNet},
		// code/run_skill are not gated by oc permissions -- Deno sandbox handles them.
		{"code", `{"code":"console.log(1)"}`, ""},
		{"run_skill", `{"name":"test"}`, ""},
		{"todowrite", `{"todos":[]}`, ""},
	}

	for _, tc := range tests {
		cat, _, _ := categorize(tc.tool, json.RawMessage(tc.input))
		if cat != tc.wantCat {
			t.Errorf("categorize(%q) = %q, want %q", tc.tool, cat, tc.wantCat)
		}
	}
}

func TestCategorize_Resource(t *testing.T) {
	tests := []struct {
		tool    string
		input   string
		wantRes string
	}{
		{"read_file", `{"path":"main.go"}`, "main.go"},
		{"bash", `{"command":"go test"}`, "go test"},
		{"webfetch", `{"url":"https://example.com/page"}`, "https://example.com/page"},
		{"write_file", `{"path":"out.txt","content":"hi"}`, "out.txt"},
	}

	for _, tc := range tests {
		_, _, res := categorize(tc.tool, json.RawMessage(tc.input))
		if res != tc.wantRes {
			t.Errorf("categorize(%q) resource = %q, want %q", tc.tool, res, tc.wantRes)
		}
	}
}

func TestPermissionSetAllowDeny(t *testing.T) {
	ps := newPermissionSet()

	if ps.Get(PermRead) != PermAllow {
		t.Error("read should default to allow")
	}

	if ps.Get(PermWrite) != PermAsk {
		t.Error("write should default to ask")
	}

	ps.Set(PermWrite, PermDeny)
	if ps.Get(PermWrite) != PermDeny {
		t.Error("write should be deny after Set")
	}

	ps.SetSession(PermWrite, PermAllow)
	if ps.Get(PermWrite) != PermAllow {
		t.Error("session override should take priority")
	}
}

func TestCheckToolPermission_AllowSkips(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.Set(PermRead, PermAllow)
	permissions.Set(PermWrite, PermAllow)
	permissions.Set(PermBash, PermAllow)
	permissions.Set(PermNet, PermAllow)

	tools := []struct {
		name  string
		input string
	}{
		{"read_file", `{"path":"main.go"}`},
		{"write_file", `{"path":"x.go","content":"hi"}`},
		{"bash", `{"command":"ls"}`},
		{"webfetch", `{"url":"https://example.com"}`},
		// code tool is not gated -- always passes through.
		{"code", `{"code":"1+1"}`},
		{"todowrite", `{"todos":[]}`},
	}

	for _, tc := range tools {
		result := CheckToolPermission(tc.name, json.RawMessage(tc.input))
		if result != nil {
			t.Errorf("CheckToolPermission(%q) should be nil when all allowed, got: %s", tc.name, result.Content)
		}
	}
}

func TestCheckToolPermission_DenyBlocks(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.Set(PermWrite, PermDeny)
	permissions.Set(PermBash, PermDeny)
	permissions.Set(PermNet, PermDeny)

	denied := []struct {
		name  string
		input string
	}{
		{"write_file", `{"path":"x.go","content":"hi"}`},
		{"edit_file", `{"path":"x.go","old_string":"a","new_string":"b"}`},
		{"bash", `{"command":"rm -rf /"}`},
		{"webfetch", `{"url":"https://evil.com"}`},
	}

	for _, tc := range denied {
		result := CheckToolPermission(tc.name, json.RawMessage(tc.input))
		if result == nil {
			t.Errorf("CheckToolPermission(%q) should be denied", tc.name)
		} else if !result.IsError {
			t.Errorf("CheckToolPermission(%q) denial should be an error", tc.name)
		}
	}
}

func TestCheckToolPermission_CodeToolNotGated(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	// Even with everything denied, code/run_skill pass through --
	// Deno sandbox handles enforcement.
	permissions = newPermissionSet()
	permissions.Set(PermRead, PermDeny)
	permissions.Set(PermWrite, PermDeny)
	permissions.Set(PermBash, PermDeny)
	permissions.Set(PermNet, PermDeny)

	for _, name := range []string{"code", "run_skill"} {
		result := CheckToolPermission(name, json.RawMessage(`{"code":"1+1"}`))
		if result != nil {
			t.Errorf("CheckToolPermission(%q) should pass through (Deno sandbox enforces), got: %s", name, result.Content)
		}
	}
}

func TestCheckToolPermission_NilPermissions(t *testing.T) {
	old := permissions
	permissions = nil
	defer func() { permissions = old }()

	result := CheckToolPermission("bash", json.RawMessage(`{"command":"ls"}`))
	if result != nil {
		t.Error("should return nil when permissions is nil")
	}
}

func TestApplyPermissionFlags(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	remaining := ApplyPermissionFlags([]string{"--allow-all", "run", "some prompt"})
	if len(remaining) != 2 || remaining[0] != "run" {
		t.Errorf("unexpected remaining: %v", remaining)
	}
	for _, cat := range allPermCategories {
		if permissions.Get(cat) != PermAllow {
			t.Errorf("--allow-all should set %s to allow", cat)
		}
	}

	permissions = newPermissionSet()
	ApplyPermissionFlags([]string{"--deny-net", "--allow-bash"})
	if permissions.Get(PermNet) != PermDeny {
		t.Error("--deny-net should set net to deny")
	}
	if permissions.Get(PermBash) != PermAllow {
		t.Error("--allow-bash should set bash to allow")
	}
}

func TestApplyPermissionFlags_Granular(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	ApplyPermissionFlags([]string{"--allow-read=./src,./lib", "--allow-bash=go,cargo", "--allow-net=example.com,*.github.com"})

	rule := permissions.getRule(PermRead)
	if rule.State != PermAllow {
		t.Error("read should be allow")
	}
	if len(rule.Scopes) != 2 || rule.Scopes[0] != "./src" || rule.Scopes[1] != "./lib" {
		t.Errorf("read scopes = %v, want [./src ./lib]", rule.Scopes)
	}

	bashRule := permissions.getRule(PermBash)
	if bashRule.State != PermAllow {
		t.Error("bash should be allow")
	}
	if len(bashRule.Scopes) != 2 || bashRule.Scopes[0] != "go" || bashRule.Scopes[1] != "cargo" {
		t.Errorf("bash scopes = %v, want [go cargo]", bashRule.Scopes)
	}

	netRule := permissions.getRule(PermNet)
	if netRule.State != PermAllow {
		t.Error("net should be allow")
	}
	if len(netRule.Scopes) != 2 {
		t.Errorf("net scopes = %v, want 2 entries", netRule.Scopes)
	}
}

func TestUncategorizedToolsAlwaysAllowed(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.Set(PermRead, PermDeny)
	permissions.Set(PermWrite, PermDeny)

	result := CheckToolPermission("todowrite", json.RawMessage(`{"todos":[]}`))
	if result != nil {
		t.Error("todowrite should always be allowed")
	}
}

// --- Scope matching tests ---

func TestMatchesPathScope(t *testing.T) {
	tests := []struct {
		path   string
		scopes []string
		want   bool
	}{
		{"src/main.go", []string{"src"}, true},
		{"src/sub/file.go", []string{"src"}, true},
		{"lib/util.go", []string{"src"}, false},
		{"main.go", []string{"."}, true},
		{"src/main.go", []string{"src", "lib"}, true},
		{"lib/x.go", []string{"src", "lib"}, true},
		{"vendor/x.go", []string{"src", "lib"}, false},
	}

	for _, tc := range tests {
		got := matchesPathScope(tc.path, tc.scopes)
		if got != tc.want {
			t.Errorf("matchesPathScope(%q, %v) = %v, want %v", tc.path, tc.scopes, got, tc.want)
		}
	}
}

func TestMatchesHostScope(t *testing.T) {
	tests := []struct {
		url    string
		scopes []string
		want   bool
	}{
		{"https://example.com/page", []string{"example.com"}, true},
		{"https://evil.com/page", []string{"example.com"}, false},
		{"https://api.github.com/repos", []string{"*.github.com"}, true},
		{"https://github.com/repos", []string{"*.github.com"}, false},
		{"https://sub.example.com/x", []string{"*.example.com", "other.com"}, true},
		{"https://other.com/y", []string{"*.example.com", "other.com"}, true},
	}

	for _, tc := range tests {
		got := matchesHostScope(tc.url, tc.scopes)
		if got != tc.want {
			t.Errorf("matchesHostScope(%q, %v) = %v, want %v", tc.url, tc.scopes, got, tc.want)
		}
	}
}

func TestMatchesCmdScope(t *testing.T) {
	tests := []struct {
		cmd    string
		scopes []string
		want   bool
	}{
		{"go test ./...", []string{"go"}, true},
		{"go build", []string{"go", "cargo"}, true},
		{"cargo test", []string{"go", "cargo"}, true},
		{"rm -rf /", []string{"go", "cargo"}, false},
		{"git push", []string{"go", "cargo"}, false},
		{"go", []string{"go"}, true},
	}

	for _, tc := range tests {
		got := matchesCmdScope(tc.cmd, tc.scopes)
		if got != tc.want {
			t.Errorf("matchesCmdScope(%q, %v) = %v, want %v", tc.cmd, tc.scopes, got, tc.want)
		}
	}
}

func TestScopedAllowRespectsScopes(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.SetWithScopes(PermWrite, PermAllow, []string{"src", "lib"})

	// Write inside scope: allowed.
	result := CheckToolPermission("write_file", json.RawMessage(`{"path":"src/main.go","content":"x"}`))
	if result != nil {
		t.Errorf("write to src/main.go should be allowed, got: %v", result)
	}

	// Verify scope matching directly.
	rule := permissions.getRule(PermWrite)
	if rule.State != PermAllow {
		t.Error("state should be allow")
	}
	if !matchesPathScope("src/main.go", rule.Scopes) {
		t.Error("src/main.go should match scope")
	}
	if matchesPathScope("vendor/bad.go", rule.Scopes) {
		t.Error("vendor/bad.go should NOT match scope")
	}
}

// --- Deno flag generation tests ---

func TestDenoPermFlags_AllAllow(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.Set(PermRead, PermAllow)
	permissions.Set(PermWrite, PermAllow)
	permissions.Set(PermBash, PermAllow)
	permissions.Set(PermNet, PermAllow)

	flags := DenoPermFlags()
	joined := strings.Join(flags, " ")
	if !strings.Contains(joined, "--allow-read=") {
		t.Error("should have --allow-read")
	}
	if !strings.Contains(joined, "--allow-write=") {
		t.Error("should have --allow-write")
	}
	if !strings.Contains(joined, "--allow-run=find,grep,bash") {
		t.Errorf("should have --allow-run=find,grep,bash, got: %s", joined)
	}
	if !strings.Contains(joined, "--allow-net") {
		t.Error("should have --allow-net")
	}
	if !strings.Contains(joined, "--allow-env=OC_ASK_CALLBACK") {
		t.Error("should have --allow-env")
	}
}

func TestDenoPermFlags_AllDeny(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.Set(PermRead, PermDeny)
	permissions.Set(PermWrite, PermDeny)
	permissions.Set(PermBash, PermDeny)
	permissions.Set(PermNet, PermDeny)

	flags := DenoPermFlags()
	joined := strings.Join(flags, " ")
	if strings.Contains(joined, "--allow-read") {
		t.Errorf("should not have --allow-read in deny mode, got: %s", joined)
	}
	if strings.Contains(joined, "--allow-write") {
		t.Errorf("should not have --allow-write in deny mode, got: %s", joined)
	}
	if strings.Contains(joined, "--allow-run") {
		t.Errorf("should not have --allow-run in deny mode, got: %s", joined)
	}
	if strings.Contains(joined, "--allow-net") {
		t.Errorf("should not have --allow-net in deny mode, got: %s", joined)
	}
	if !strings.Contains(joined, "--allow-env=OC_ASK_CALLBACK") {
		t.Error("should always have --allow-env for callback")
	}
}

func TestDenoPermFlags_ScopedRead(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.SetWithScopes(PermRead, PermAllow, []string{"./src", "./lib"})
	permissions.Set(PermWrite, PermDeny)
	permissions.Set(PermBash, PermDeny)
	permissions.Set(PermNet, PermDeny)

	flags := DenoPermFlags()
	joined := strings.Join(flags, " ")
	if !strings.Contains(joined, "--allow-read=") {
		t.Error("should have scoped --allow-read")
	}
	if !strings.Contains(joined, "/tmp") {
		t.Errorf("should include /tmp in read scopes, got: %s", joined)
	}
}

func TestDenoPermFlags_ScopedBash(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.SetWithScopes(PermBash, PermAllow, []string{"go", "cargo"})
	permissions.Set(PermRead, PermAllow)
	permissions.Set(PermWrite, PermDeny)
	permissions.Set(PermNet, PermDeny)

	flags := DenoPermFlags()
	joined := strings.Join(flags, " ")
	if !strings.Contains(joined, "--allow-run=") {
		t.Errorf("should have --allow-run, got: %s", joined)
	}
	if !strings.Contains(joined, "find") || !strings.Contains(joined, "grep") {
		t.Errorf("should include find,grep in --allow-run, got: %s", joined)
	}
	if !strings.Contains(joined, "go") || !strings.Contains(joined, "cargo") {
		t.Errorf("should include go,cargo in --allow-run, got: %s", joined)
	}
	runFlag := ""
	for _, f := range flags {
		if strings.HasPrefix(f, "--allow-run=") {
			runFlag = f
		}
	}
	if strings.Contains(runFlag, "bash") {
		t.Errorf("scoped bash should not include 'bash' binary, got: %s", runFlag)
	}
}

func TestDenoPermFlags_ScopedNet(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	permissions = newPermissionSet()
	permissions.SetWithScopes(PermNet, PermAllow, []string{"example.com", "api.github.com"})

	flags := DenoPermFlags()
	joined := strings.Join(flags, " ")
	if !strings.Contains(joined, "--allow-net=example.com,api.github.com") {
		t.Errorf("should have scoped --allow-net, got: %s", joined)
	}
}

func TestDenoPermFlags_NilPermissions(t *testing.T) {
	old := permissions
	permissions = nil
	defer func() { permissions = old }()

	flags := DenoPermFlags()
	if len(flags) == 0 {
		t.Error("nil permissions should still return default flags")
	}
	joined := strings.Join(flags, " ")
	if !strings.Contains(joined, "--allow-read") {
		t.Error("fallback should have --allow-read")
	}
}

func TestDenoPermFlags_AskMode(t *testing.T) {
	old := permissions
	defer func() { permissions = old }()

	// Default PermissionSet: read=allow, write/bash/net=ask.
	permissions = newPermissionSet()
	flags := DenoPermFlags()
	joined := strings.Join(flags, " ")

	if !strings.Contains(joined, "--allow-read=") {
		t.Error("read=allow should produce --allow-read")
	}
	if !strings.Contains(joined, "--allow-write=") {
		t.Error("write=ask should produce conservative --allow-write")
	}
	// Bash: ask -> only find/grep (no bash binary).
	runFlag := ""
	for _, f := range flags {
		if strings.HasPrefix(f, "--allow-run=") {
			runFlag = f
		}
	}
	if runFlag == "" {
		t.Error("bash=ask should still allow find,grep")
	} else if strings.Contains(runFlag, "bash") {
		t.Errorf("bash=ask should NOT allow bash binary, got: %s", runFlag)
	}
	if strings.Contains(joined, "--allow-net") {
		t.Error("net=ask should not produce --allow-net")
	}
}
