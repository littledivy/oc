package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadCache_HitMiss(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.txt")
	os.WriteFile(f, []byte("hello world"), 0o644)

	rc := NewReadCache()

	_, hit := rc.Get(f)
	if hit {
		t.Fatal("expected miss on first read")
	}
	if rc.misses != 1 {
		t.Fatalf("expected 1 miss, got %d", rc.misses)
	}

	rc.Put(f, "hello world")
	content, hit := rc.Get(f)
	if !hit {
		t.Fatal("expected hit after put")
	}
	if content != "hello world" {
		t.Fatalf("expected 'hello world', got %q", content)
	}
	if rc.hits != 1 {
		t.Fatalf("expected 1 hit, got %d", rc.hits)
	}
}

func TestReadCache_InvalidateOnModify(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.txt")
	os.WriteFile(f, []byte("v1"), 0o644)

	rc := NewReadCache()
	rc.Put(f, "v1")

	os.WriteFile(f, []byte("v2 longer"), 0o644)

	_, hit := rc.Get(f)
	if hit {
		t.Fatal("expected miss after file modification")
	}
}

func TestReadCache_ExplicitInvalidate(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.txt")
	os.WriteFile(f, []byte("hello"), 0o644)

	rc := NewReadCache()
	rc.Put(f, "hello")

	rc.Invalidate(f)

	_, hit := rc.Get(f)
	if hit {
		t.Fatal("expected miss after invalidate")
	}
}

func TestBashMemo_HitMiss(t *testing.T) {
	bm := NewBashMemo()

	_, hit := bm.Get("ls -la")
	if hit {
		t.Fatal("expected miss on first get")
	}

	bm.Put("ls -la", "file1\nfile2\n")

	output, hit := bm.Get("ls -la")
	if !hit {
		t.Fatal("expected hit after put")
	}
	if output != "file1\nfile2\n" {
		t.Fatalf("unexpected output: %q", output)
	}
}

func TestBashMemo_InvalidateAll(t *testing.T) {
	bm := NewBashMemo()
	bm.Put("ls", "output1")
	bm.Put("git status", "output2")

	bm.InvalidateAll()

	_, hit := bm.Get("ls")
	if hit {
		t.Fatal("expected miss after invalidate all")
	}
}

func TestBashMemo_LRUEviction(t *testing.T) {
	bm := NewBashMemo()

	for i := 0; i < bashMemoMaxEntries+5; i++ {
		cmd := "ls " + string(rune('a'+i%26)) + string(rune('0'+i/26))
		bm.Put(cmd, "out")
	}

	if len(bm.entries) > bashMemoMaxEntries {
		t.Fatalf("expected at most %d entries, got %d", bashMemoMaxEntries, len(bm.entries))
	}
}

func TestIsReadOnlyBash(t *testing.T) {
	tests := []struct {
		cmd    string
		expect bool
	}{
		{"ls", true},
		{"ls -la", true},
		{"git status", true},
		{"git log --oneline", true},
		{"git diff HEAD", true},
		{"grep -r foo .", true},
		{"rg pattern", true},
		{"cat file.txt", true},
		{"head -20 file.txt", true},
		{"wc -l file.txt", true},
		{"tree", true},
		{"which go", true},
		{"echo hello", true},
		{"rm -rf /tmp/foo", false},
		{"echo hello > file.txt", false},
		{"go build", false},
		{"sed -i 's/a/b/' file.txt", false},
		{"mv a b", false},
		{"cp a b", false},
		{"mkdir -p dir", false},
		{"npm install", false},
	}

	for _, tt := range tests {
		got := isReadOnlyBash(tt.cmd)
		if got != tt.expect {
			t.Errorf("isReadOnlyBash(%q) = %v, want %v", tt.cmd, got, tt.expect)
		}
	}
}

func TestNormalizeBashCmd(t *testing.T) {
	tests := []struct {
		input, expect string
	}{
		{"  ls   -la  ", "ls -la"},
		{"git  status", "git status"},
		{"cat\tfile.txt", "cat file.txt"},
	}
	for _, tt := range tests {
		got := normalizeBashCmd(tt.input)
		if got != tt.expect {
			t.Errorf("normalizeBashCmd(%q) = %q, want %q", tt.input, got, tt.expect)
		}
	}
}

func TestExtractToolFilePath(t *testing.T) {
	input := json.RawMessage(`{"path": "/tmp/test.go"}`)
	got := extractToolFilePath("read_file", input)
	if got != "/tmp/test.go" {
		t.Errorf("expected /tmp/test.go, got %q", got)
	}

	input2 := json.RawMessage(`{"file_path": "/tmp/other.go"}`)
	got2 := extractToolFilePath("read_file", input2)
	if got2 != "/tmp/other.go" {
		t.Errorf("expected /tmp/other.go, got %q", got2)
	}
}

func TestExtractBashCommand(t *testing.T) {
	input := json.RawMessage(`{"command": "ls -la"}`)
	got := extractBashCommand(input)
	if got != "ls -la" {
		t.Errorf("expected 'ls -la', got %q", got)
	}
}

func TestSessionCache_Stats(t *testing.T) {
	sc := NewSessionCache()

	if s := sc.Stats(); s != "" {
		t.Errorf("expected empty stats, got %q", s)
	}

	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.txt")
	os.WriteFile(f, []byte("content"), 0o644)

	sc.Reads.Get(f) // miss
	sc.Reads.Put(f, "content")
	sc.Reads.Get(f) // hit

	s := sc.Stats()
	if s == "" {
		t.Fatal("expected non-empty stats")
	}
	if !strings.Contains(s, "1 hits") {
		t.Errorf("expected '1 hits' in stats: %q", s)
	}
}

func TestIsReadOnlyPipeline(t *testing.T) {
	tests := []struct {
		cmd    string
		expect bool
	}{
		{"ls | grep foo", true},
		{"cat file.txt | head -10", true},
		{"git log | grep fix | wc -l", true},
		{"ls | rm", false}, // rm is not read-only
	}
	for _, tt := range tests {
		got := isReadOnlyBash(tt.cmd)
		if got != tt.expect {
			t.Errorf("isReadOnlyBash(%q) = %v, want %v", tt.cmd, got, tt.expect)
		}
	}
}

func TestIsBuildBash(t *testing.T) {
	tests := []struct {
		cmd    string
		expect bool
	}{
		{"cargo build --bin deno", true},
		{"cargo test -p deno_node", true},
		{"cargo check -p deno_ffi", true},
		{"cargo b -p deno", true},
		{"go build ./...", true},
		{"go test ./...", true},
		{"deno test --no-check", true},
		{"nix build 2>&1", true},
		{"make clean", true},
		{"ls", false},
		{"git status", false},
		{"echo hello", false},
		{"cat file.go", false},
	}
	for _, tt := range tests {
		got := isBuildBash(tt.cmd)
		if got != tt.expect {
			t.Errorf("isBuildBash(%q) = %v, want %v", tt.cmd, got, tt.expect)
		}
	}
}

func TestEditFile_Success(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.go")
	os.WriteFile(f, []byte("func Add(a, b int) int { return a + b }"), 0o644)

	result := execEditFile(f, "return a + b", "return a + b + 0")
	if result.IsError {
		t.Fatalf("expected success, got error: %s", result.Content)
	}

	data, _ := os.ReadFile(f)
	if !strings.Contains(string(data), "return a + b + 0") {
		t.Fatalf("file not updated: %s", string(data))
	}
}

func TestEditFile_NoMatch(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.go")
	os.WriteFile(f, []byte("func Add(a, b int) int { return a + b }"), 0o644)

	result := execEditFile(f, "this does not exist", "replacement")
	if !result.IsError {
		t.Fatal("expected error for non-matching old_string")
	}
	if !strings.Contains(result.Content, "old_string not found") {
		t.Fatalf("expected 'not found' error, got: %s", result.Content)
	}
}

func TestEditFile_MultipleMatches(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.go")
	os.WriteFile(f, []byte("aaa bbb aaa"), 0o644)

	result := execEditFile(f, "aaa", "ccc")
	if !result.IsError {
		t.Fatal("expected error for multiple matches")
	}
	if !strings.Contains(result.Content, "matches 2 locations") {
		t.Fatalf("expected multiple match error, got: %s", result.Content)
	}
}

func TestEditFile_IdenticalStrings(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.go")
	os.WriteFile(f, []byte("hello"), 0o644)

	result := execEditFile(f, "hello", "hello")
	if !result.IsError {
		t.Fatal("expected error for identical strings")
	}
}

func TestEditFile_FuzzyHint(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.go")
	os.WriteFile(f, []byte("func Add(a, b int) int {\n\treturn a + b\n}\n"), 0o644)

	result := execEditFile(f, "  return a + b", "  return a + b + 0")
	if !result.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(result.Content, "Did you mean") {
		t.Fatalf("expected fuzzy hint, got: %s", result.Content)
	}
}

func TestLongestCommonSubstring(t *testing.T) {
	if longestCommonSubstring("abcdef", "xbcdy") != 3 {
		t.Error("expected 3 for 'bcd'")
	}
	if longestCommonSubstring("", "abc") != 0 {
		t.Error("expected 0 for empty string")
	}
	if longestCommonSubstring("abc", "abc") != 3 {
		t.Error("expected 3 for identical strings")
	}
}

func TestEstimateTokens(t *testing.T) {
	if estimateTokens("") != 0 {
		t.Error("empty string should be 0 tokens")
	}
	tok := estimateTokens("hello")
	if tok < 1 || tok > 3 {
		t.Errorf("expected ~2 tokens for 'hello', got %d", tok)
	}
}
