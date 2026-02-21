package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseCompilerErrors_Go(t *testing.T) {
	output := `./main.go:42:15: undefined: foo
./main.go:50:3: syntax error: unexpected }
./util.go:10:1: imported and not used: "fmt"
`
	errors := ParseCompilerErrors(output, "go")
	if len(errors) != 3 {
		t.Fatalf("expected 3 errors, got %d", len(errors))
	}

	if errors[0].File != "./main.go" || errors[0].Line != 42 || errors[0].Col != 15 {
		t.Errorf("error[0] = %+v", errors[0])
	}
	if errors[0].Message != "undefined: foo" {
		t.Errorf("message = %q", errors[0].Message)
	}
}

func TestParseCompilerErrors_Rust(t *testing.T) {
	output := `error[E0425]: cannot find value x
 --> src/main.rs:10:5
  |
10 |     x + 1
  |     ^ not found
error[E0308]: mismatched types
 --> src/lib.rs:20:10
`
	errors := ParseCompilerErrors(output, "rust")
	if len(errors) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(errors))
	}
	if errors[0].File != "src/main.rs" || errors[0].Line != 10 {
		t.Errorf("error[0] = %+v", errors[0])
	}
	if errors[1].File != "src/lib.rs" || errors[1].Line != 20 {
		t.Errorf("error[1] = %+v", errors[1])
	}
}

func TestParseCompilerErrors_TypeScript(t *testing.T) {
	output := `src/index.ts(10,5): error TS2304: Cannot find name 'x'.
src/index.ts(20,3): error TS1005: ';' expected.`
	errors := ParseCompilerErrors(output, "node")
	if len(errors) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(errors))
	}
	if errors[0].File != "src/index.ts" || errors[0].Line != 10 {
		t.Errorf("error[0] = %+v", errors[0])
	}
}

func TestParseCompilerErrors_Dedup(t *testing.T) {
	output := `./main.go:10:5: error one
./main.go:10:5: error two
`
	errors := ParseCompilerErrors(output, "go")
	if len(errors) != 1 {
		t.Fatalf("expected 1 deduped error, got %d", len(errors))
	}
}

func TestParseCompilerErrors_MaxFive(t *testing.T) {
	var lines []string
	for i := 1; i <= 10; i++ {
		lines = append(lines, "./main.go:"+strings.Repeat("1", 1)+":1: error "+string(rune('a'+i)))
	}
	output := ""
	for i := 1; i <= 10; i++ {
		output += "./main.go:" + strconv.Itoa(i*10) + ":1: error\n"
	}
	errors := ParseCompilerErrors(output, "go")
	if len(errors) > 5 {
		t.Fatalf("expected max 5 errors, got %d", len(errors))
	}
}

func TestReadSourceContext(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "test.go")

	var content strings.Builder
	for i := 1; i <= 20; i++ {
		content.WriteString("line " + strconv.Itoa(i) + "\n")
	}
	os.WriteFile(f, []byte(content.String()), 0o644)

	snippet := readSourceContext(f, 10, 2)
	if snippet == "" {
		t.Fatal("expected non-empty snippet")
	}
	if !strings.Contains(snippet, "> ") {
		t.Fatal("expected marker on target line")
	}
	if !strings.Contains(snippet, "line 10") {
		t.Fatal("expected target line content")
	}
	if !strings.Contains(snippet, "line 8") || !strings.Contains(snippet, "line 12") {
		t.Fatal("expected context lines")
	}
}

func TestReadSourceContext_EdgeCases(t *testing.T) {
	if readSourceContext("/nonexistent/file.go", 10, 3) != "" {
		t.Fatal("expected empty for missing file")
	}

	tmp := t.TempDir()
	f := filepath.Join(tmp, "small.go")
	os.WriteFile(f, []byte("line1\nline2\n"), 0o644)
	if readSourceContext(f, 100, 3) != "" {
		t.Fatal("expected empty for out-of-range line")
	}
}

func TestEnrichBuildErrors(t *testing.T) {
	tmp := t.TempDir()
	f := filepath.Join(tmp, "main.go")
	os.WriteFile(f, []byte("package main\n\nfunc main() {\n\tfoo()\n}\n"), 0o644)

	buildResult := &AutoBuildResult{
		Output:  f + ":4:2: undefined: foo\n",
		IsError: true,
		Elapsed: 100 * time.Millisecond,
		Command: "go build ./...",
	}

	enriched := EnrichBuildErrors(buildResult, "go")
	if enriched == "" {
		t.Fatal("expected enriched output")
	}
	if !strings.Contains(enriched, "error context") {
		t.Fatal("expected 'error context' header")
	}
	if !strings.Contains(enriched, "foo()") {
		t.Fatal("expected source context with foo()")
	}
}

func TestEnrichBuildErrors_NilOrSuccess(t *testing.T) {
	if EnrichBuildErrors(nil, "go") != "" {
		t.Fatal("expected empty for nil")
	}
	if EnrichBuildErrors(&AutoBuildResult{Output: "ok", IsError: false}, "go") != "" {
		t.Fatal("expected empty for success")
	}
}

func TestEnrichBuildErrors_NoErrors(t *testing.T) {
	result := &AutoBuildResult{
		Output:  "some random output with no parseable errors\n",
		IsError: true,
	}
	if EnrichBuildErrors(result, "go") != "" {
		t.Fatal("expected empty when no errors parsed")
	}
}
