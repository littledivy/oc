package main

import (
	"encoding/json"
	"testing"
)

func TestCanonicalSignature(t *testing.T) {
	ops := []IRop{
		{Kind: OpRead, Tool: "read_file"},
		{Kind: OpRead, Tool: "read_file"},
		{Kind: OpWrite, Tool: "write_file"},
		{Kind: OpExec, Tool: "bash"},
	}
	sig := CanonicalSignature(ops)
	if sig["read:read_file"] != 2 {
		t.Errorf("expected read:read_file=2, got %d", sig["read:read_file"])
	}
	if sig["write:write_file"] != 1 {
		t.Errorf("expected write:write_file=1, got %d", sig["write:write_file"])
	}
	if sig["exec:bash"] != 1 {
		t.Errorf("expected exec:bash=1, got %d", sig["exec:bash"])
	}
}

func TestCanonicalSignatureEmpty(t *testing.T) {
	sig := CanonicalSignature(nil)
	if len(sig) != 0 {
		t.Errorf("expected empty sig, got %v", sig)
	}
}

func TestSignatureOverlap(t *testing.T) {
	a := map[string]int{"read:read_file": 2, "write:write_file": 1, "exec:bash": 1}
	b := map[string]int{"read:read_file": 2, "write:write_file": 1, "exec:bash": 1}
	if overlap := SignatureOverlap(a, b); overlap != 1.0 {
		t.Errorf("identical sigs: expected 1.0, got %.2f", overlap)
	}
}

func TestSignatureOverlapPartial(t *testing.T) {
	a := map[string]int{"read:read_file": 2, "write:write_file": 1}
	b := map[string]int{"read:read_file": 2, "exec:bash": 1}
	overlap := SignatureOverlap(a, b)
	if overlap != 0.5 {
		t.Errorf("expected 0.5, got %.2f", overlap)
	}
}

func TestSignatureOverlapEmpty(t *testing.T) {
	if overlap := SignatureOverlap(nil, nil); overlap != 0 {
		t.Errorf("expected 0, got %.2f", overlap)
	}
	a := map[string]int{"read:read_file": 1}
	if overlap := SignatureOverlap(a, nil); overlap != 0 {
		t.Errorf("expected 0, got %.2f", overlap)
	}
}

func TestExtractArgs(t *testing.T) {
	raw := json.RawMessage(`{"path": "/tmp/foo.go", "content": "hello"}`)
	args := extractArgs(raw)
	if args["path"] != "/tmp/foo.go" {
		t.Errorf("expected /tmp/foo.go, got %s", args["path"])
	}
	if args["content"] != "hello" {
		t.Errorf("expected hello, got %s", args["content"])
	}
}

func TestExtractArgsInvalid(t *testing.T) {
	args := extractArgs(json.RawMessage(`invalid`))
	if args != nil {
		t.Errorf("expected nil for invalid JSON, got %v", args)
	}
}

func TestMergeEmbeddings(t *testing.T) {
	a := []float64{1.0, 0.0, 0.5}
	b := []float64{0.0, 1.0, 0.5}
	result := mergeEmbeddings(a, b, 2)
	if len(result) != 3 {
		t.Fatalf("expected len 3, got %d", len(result))
	}
	if result[0] != 0.5 {
		t.Errorf("expected 0.5, got %f", result[0])
	}
	if result[1] != 0.5 {
		t.Errorf("expected 0.5, got %f", result[1])
	}
	if result[2] != 0.5 {
		t.Errorf("expected 0.5, got %f", result[2])
	}
}

func TestMergeEmbeddingsEmpty(t *testing.T) {
	b := []float64{1.0, 2.0}
	result := mergeEmbeddings(nil, b, 1)
	if len(result) != 2 || result[0] != 1.0 {
		t.Errorf("expected b returned when existing is nil, got %v", result)
	}
}

func TestArgStabilityTracking(t *testing.T) {
	ps := &PatternStore{}

	trace1 := &IRTrace{
		Trigger:   "add test for subtract",
		Signature: "abc123",
		Ops: []IRop{
			{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)},
			{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc_test.go"}`)},
			{Kind: OpWrite, Tool: "write_file", Args: json.RawMessage(`{"path":"calc_test.go","content":"test subtract"}`)},
			{Kind: OpAssert, Tool: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`)},
		},
	}

	ps.LearnPattern(trace1)
	if len(ps.Patterns) != 1 {
		t.Fatalf("expected 1 pattern, got %d", len(ps.Patterns))
	}
	p := ps.Patterns[0]
	if p.Occurrences != 1 {
		t.Errorf("expected 1 occurrence, got %d", p.Occurrences)
	}
	for _, op := range p.Ops {
		if op.Stability != 1.0 {
			t.Errorf("first trace: expected stability 1.0 for %s, got %.2f", op.Tool, op.Stability)
		}
	}

	trace2 := &IRTrace{
		Trigger:   "add test for multiply",
		Signature: "abc123",
		Ops: []IRop{
			{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)},
			{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc_test.go"}`)},
			{Kind: OpWrite, Tool: "write_file", Args: json.RawMessage(`{"path":"calc_test.go","content":"test multiply"}`)},
			{Kind: OpAssert, Tool: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`)},
		},
	}

	sig1 := CanonicalSignature(trace1.Ops)
	sig2 := CanonicalSignature(trace2.Ops)
	if SignatureOverlap(sig1, sig2) != 1.0 {
		t.Fatalf("expected identical canonical signatures")
	}

	p.Signature = sig1

	ps.LearnPattern(trace2)
	if len(ps.Patterns) != 1 {
		t.Fatalf("expected patterns to merge, got %d", len(ps.Patterns))
	}

	p = ps.Patterns[0]
	if p.Occurrences != 2 {
		t.Errorf("expected 2 occurrences, got %d", p.Occurrences)
	}

	for _, op := range p.Ops {
		if op.Tool == "read_file" {
			if op.StableArgs["path"] == "" {
				t.Errorf("read_file path should be stable")
			}
			if op.Stability != 1.0 {
				t.Errorf("read_file stability should be 1.0, got %.2f", op.Stability)
			}
		}
		if op.Tool == "write_file" {
			if _, ok := op.StableArgs["content"]; ok {
				t.Errorf("write_file content should NOT be stable (differs between traces)")
			}
			if op.StableArgs["path"] != "calc_test.go" {
				t.Errorf("write_file path should be stable as calc_test.go")
			}
		}
		if op.Tool == "bash" {
			if op.StableArgs["command"] != "go test ./..." {
				t.Errorf("bash command should be stable")
			}
		}
	}
}

func TestParseLLMClassification(t *testing.T) {
	patterns := map[string]*Pattern{
		"abc123": {ID: "abc123", Keywords: []string{"add", "test"}},
	}

	resp := `{"pattern": "abc123", "confidence": 0.95, "params": {"function_name": "Divide"}}`
	result := parseLLMClassification(resp, patterns)
	if result == nil {
		t.Fatal("expected match, got nil")
	}
	if result.Pattern.ID != "abc123" {
		t.Errorf("expected abc123, got %s", result.Pattern.ID)
	}
	if result.Confidence != 0.95 {
		t.Errorf("expected 0.95, got %f", result.Confidence)
	}
	if result.Params["function_name"] != "Divide" {
		t.Errorf("expected Divide, got %s", result.Params["function_name"])
	}

	resp2 := `{"pattern": "none"}`
	if parseLLMClassification(resp2, patterns) != nil {
		t.Error("expected nil for 'none' pattern")
	}

	resp3 := `{"pattern": "unknown", "confidence": 0.9}`
	if parseLLMClassification(resp3, patterns) != nil {
		t.Error("expected nil for unknown pattern ID")
	}

	if parseLLMClassification("not json", patterns) != nil {
		t.Error("expected nil for invalid JSON")
	}

	resp4 := `Here is my answer: {"pattern": "abc123", "confidence": 0.85, "params": {}} end`
	result4 := parseLLMClassification(resp4, patterns)
	if result4 == nil {
		t.Fatal("expected match from embedded JSON")
	}
	if result4.Confidence != 0.85 {
		t.Errorf("expected 0.85, got %f", result4.Confidence)
	}
}

func TestTraceIndexSearchEmpty(t *testing.T) {
	ti := &TraceIndex{}
	results := ti.Search([]float64{1, 0, 0}, 3)
	if len(results) != 0 {
		t.Errorf("expected 0 results from empty index, got %d", len(results))
	}

	results = ti.Search(nil, 3)
	if len(results) != 0 {
		t.Errorf("expected 0 results for nil embedding, got %d", len(results))
	}
}

func TestTraceIndexSearch(t *testing.T) {
	ti := &TraceIndex{
		Entries: []TraceIndexEntry{
			{TriggerID: "a", Trigger: "add test subtract", Signature: "sig1", Embedding: []float64{0.9, 0.1, 0.0}},
			{TriggerID: "b", Trigger: "fix bug login", Signature: "sig2", Embedding: []float64{0.1, 0.9, 0.0}},
			{TriggerID: "c", Trigger: "add test multiply", Signature: "sig1", Embedding: []float64{0.85, 0.15, 0.0}},
		},
	}

	results := ti.Search([]float64{1.0, 0.0, 0.0}, 2)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].TriggerID != "a" {
		t.Errorf("expected nearest to be 'a', got %s", results[0].TriggerID)
	}
	if results[1].TriggerID != "c" {
		t.Errorf("expected second nearest to be 'c', got %s", results[1].TriggerID)
	}
}

func TestTraceIndexAddNoDuplicate(t *testing.T) {
	ti := &TraceIndex{}
	ti.Add("add test subtract", "sig1", []IRop{{Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)}})
	ti.Add("add test subtract", "sig1", []IRop{{Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)}})
	if len(ti.Entries) != 1 {
		t.Errorf("expected 1 entry (no duplicates), got %d", len(ti.Entries))
	}
}

func TestPredictedOpsToIROps(t *testing.T) {
	ops := []PredictedOp{
		{Tool: "read_file", Kind: OpRead, StableArgs: map[string]string{"path": "calc.go"}},
		{Tool: "bash", Kind: OpAssert, StableArgs: map[string]string{"command": "go test"}},
	}
	irOps := predictedOpsToIROps(ops)
	if len(irOps) != 2 {
		t.Fatalf("expected 2 ops, got %d", len(irOps))
	}
	if irOps[0].Tool != "read_file" || irOps[0].Kind != OpRead {
		t.Errorf("unexpected first op: %v", irOps[0])
	}
	if irOps[1].Tool != "bash" || irOps[1].Kind != OpAssert {
		t.Errorf("unexpected second op: %v", irOps[1])
	}
}
