package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func makeEmbedding(seed int, dim int) []float64 {
	r := rand.New(rand.NewSource(int64(seed)))
	emb := make([]float64, dim)
	for i := range emb {
		emb[i] = r.Float64()
	}
	return emb
}

func makePattern(id int, nOps int, embDim int) *Pattern {
	ops := make([]PredictedOp, nOps)
	tools := []string{"read_file", "write_file", "bash", "list_files"}
	kinds := []OpKind{OpRead, OpWrite, OpExec, OpQuery}
	for i := range ops {
		stable := map[string]string{"path": fmt.Sprintf("file_%d_%d.go", id, i)}
		ops[i] = PredictedOp{
			Tool:       tools[i%len(tools)],
			Kind:       kinds[i%len(kinds)],
			StableArgs: stable,
			TotalArgs:  1,
			SeenCount:  3,
			Stability:  1.0,
		}
	}
	sig := make(map[string]int)
	for _, op := range ops {
		sig[string(op.Kind)+":"+op.Tool]++
	}
	return &Pattern{
		ID:          fmt.Sprintf("pat_%d", id),
		Keywords:    []string{"test", "add", fmt.Sprintf("word%d", id)},
		Ops:         ops,
		Occurrences: 5,
		Successes:   3,
		LastUsed:    time.Now(),
		Embedding:   makeEmbedding(id, embDim),
		Signature:   sig,
	}
}

func BenchmarkCanonicalSignature(b *testing.B) {
	for _, nOps := range []int{4, 10, 50} {
		ops := make([]IRop, nOps)
		tools := []string{"read_file", "write_file", "bash", "list_files"}
		kinds := []OpKind{OpRead, OpWrite, OpExec, OpQuery}
		for i := range ops {
			ops[i] = IRop{Kind: kinds[i%4], Tool: tools[i%4]}
		}
		b.Run(fmt.Sprintf("ops=%d", nOps), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				CanonicalSignature(ops)
			}
		})
	}
}

func BenchmarkSignatureOverlap(b *testing.B) {
	for _, nKeys := range []int{4, 10, 50} {
		a := make(map[string]int, nKeys)
		bm := make(map[string]int, nKeys)
		for i := 0; i < nKeys; i++ {
			key := fmt.Sprintf("kind%d:tool%d", i%4, i%4)
			a[key] = i%3 + 1
			bm[key] = i%3 + 1
		}
		b.Run(fmt.Sprintf("keys=%d", nKeys), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				SignatureOverlap(a, bm)
			}
		})
	}
}

func BenchmarkCosineSimilarity(b *testing.B) {
	for _, dim := range []int{384, 768, 1536} {
		a := makeEmbedding(1, dim)
		bv := makeEmbedding(2, dim)
		b.Run(fmt.Sprintf("dim=%d", dim), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				cosineSimilarity(a, bv)
			}
		})
	}
}

func BenchmarkExtractArgs(b *testing.B) {
	small := json.RawMessage(`{"path":"calc.go"}`)
	medium := json.RawMessage(`{"path":"calc.go","content":"some content here","encoding":"utf-8"}`)
	large := json.RawMessage(`{"path":"calc.go","content":"` + string(make([]byte, 1000)) + `","encoding":"utf-8","mode":"0644","backup":"true"}`)

	b.Run("small/1key", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			extractArgs(small)
		}
	})
	b.Run("medium/3keys", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			extractArgs(medium)
		}
	})
	b.Run("large/5keys", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			extractArgs(large)
		}
	})
}

func BenchmarkMatchByEmbedding(b *testing.B) {
	for _, nPatterns := range []int{5, 20, 100} {
		patterns := make([]*Pattern, nPatterns)
		for i := range patterns {
			patterns[i] = makePattern(i, 4, 384)
			patterns[i].Occurrences = 3 // eligible
		}
		query := makeEmbedding(9999, 384)

		b.Run(fmt.Sprintf("patterns=%d", nPatterns), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var bestSim float64
				for _, p := range patterns {
					if len(p.Embedding) == 0 || p.Occurrences < 2 {
						continue
					}
					sim := cosineSimilarity(query, p.Embedding)
					if sim > bestSim {
						bestSim = sim
					}
				}
			}
		})
	}
}

func BenchmarkMergeOps(b *testing.B) {
	for _, nOps := range []int{4, 10, 20} {
		b.Run(fmt.Sprintf("ops=%d", nOps), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				p := makePattern(0, nOps, 0)
				newOps := make([]IRop, nOps)
				tools := []string{"read_file", "write_file", "bash", "list_files"}
				kinds := []OpKind{OpRead, OpWrite, OpExec, OpQuery}
				for j := range newOps {
					args := map[string]string{"path": fmt.Sprintf("file_0_%d.go", j)}
					argsJSON, _ := json.Marshal(args)
					newOps[j] = IRop{
						Tool: tools[j%len(tools)],
						Kind: kinds[j%len(kinds)],
						Args: json.RawMessage(argsJSON),
					}
				}
				b.StartTimer()
				p.mergeOps(newOps)
			}
		})
	}
}

func BenchmarkLearnPattern_New(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ps := &PatternStore{}
		trace := &IRTrace{
			Trigger:   "add test for subtract",
			Signature: "abc123",
			Ops: []IRop{
				{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)},
				{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc_test.go"}`)},
				{Kind: OpWrite, Tool: "write_file", Args: json.RawMessage(`{"path":"calc_test.go","content":"test"}`)},
				{Kind: OpAssert, Tool: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`)},
			},
		}
		b.StartTimer()
		ps.LearnPattern(trace)
	}
}

func BenchmarkLearnPattern_Merge(b *testing.B) {
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		ps := &PatternStore{}
		trace1 := &IRTrace{
			Trigger:   "add test for subtract",
			Signature: "abc123",
			Ops: []IRop{
				{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)},
				{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc_test.go"}`)},
				{Kind: OpWrite, Tool: "write_file", Args: json.RawMessage(`{"path":"calc_test.go","content":"test sub"}`)},
				{Kind: OpAssert, Tool: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`)},
			},
		}
		ps.LearnPattern(trace1)
		sig := CanonicalSignature(trace1.Ops)
		ps.Patterns[0].Signature = sig

		trace2 := &IRTrace{
			Trigger:   "add test for multiply",
			Signature: "abc123",
			Ops: []IRop{
				{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc.go"}`)},
				{Kind: OpRead, Tool: "read_file", Args: json.RawMessage(`{"path":"calc_test.go"}`)},
				{Kind: OpWrite, Tool: "write_file", Args: json.RawMessage(`{"path":"calc_test.go","content":"test mul"}`)},
				{Kind: OpAssert, Tool: "bash", Args: json.RawMessage(`{"command":"go test ./..."}`)},
			},
		}
		b.StartTimer()
		ps.LearnPattern(trace2)
	}
}

func BenchmarkParseLLMClassification(b *testing.B) {
	patterns := map[string]*Pattern{
		"abc123": {ID: "abc123", Keywords: []string{"add", "test"}},
		"def456": {ID: "def456", Keywords: []string{"fix", "bug"}},
	}

	b.Run("valid", func(b *testing.B) {
		resp := `{"pattern": "abc123", "confidence": 0.95, "params": {"function_name": "Divide"}}`
		for i := 0; i < b.N; i++ {
			parseLLMClassification(resp, patterns)
		}
	})

	b.Run("embedded_json", func(b *testing.B) {
		resp := `Here is my analysis: {"pattern": "abc123", "confidence": 0.85, "params": {"fn": "Add"}} That's all.`
		for i := 0; i < b.N; i++ {
			parseLLMClassification(resp, patterns)
		}
	})

	b.Run("none", func(b *testing.B) {
		resp := `{"pattern": "none"}`
		for i := 0; i < b.N; i++ {
			parseLLMClassification(resp, patterns)
		}
	})
}
