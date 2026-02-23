package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Tier represents an optimization level.
type Tier int

const (
	Tier0 Tier = 0 // Full LLM (interpreter)
	Tier1 Tier = 1 // LLM with knowledge preamble (fewer discovery calls)
	Tier2 Tier = 2 // Speculative execution + LLM (fewer round trips)
	Tier3 Tier = 3 // Deterministic script (0 tokens)
)

// JIT is the main optimization engine.
// It decides which tier to use for each request and learns from every interaction.
type JIT struct {
	Knowledge     *Knowledge
	Patterns      *PatternStore
	Scripts       *ScriptStore
	CodeSkills    *CodeSkillStore
	TraceIndex    *TraceIndex
	NeedTemplates *NeedTemplateStore
}

// ExecutionPlan is what the JIT produces after analyzing a prompt.
type ExecutionPlan struct {
	Tier            Tier
	Script          *Script           // non-nil for Tier 3
	Pattern         *Pattern          // non-nil for Tier 2
	SpecResults     []SpecResult      // pre-fetched results for Tier 2 (old path)
	MatchParams     map[string]string // extracted params from AI matching
	MatchStrategy   string            // matcher path used in Tier 2
	MatchConfidence float64           // confidence from matcher
	Preamble        string            // knowledge preamble (Tier 1+)
	NeedContext     string            // assembled context from NeedTemplate resolution (Tier 2)
	ResolvedNeeds   []ResolvedNeed    // resolved needs for tracing
}

type executionIntentDecision struct {
	ExecuteNow bool
	Exclusive  bool
	CommandFit bool
	Confidence float64
	ExpiresAt  time.Time
}

const executionIntentThreshold = 0.65

var executionIntentCache = struct {
	sync.RWMutex
	byKey map[string]executionIntentDecision
}{
	byKey: make(map[string]executionIntentDecision),
}


func NewJIT() *JIT {
	k := LoadKnowledge()
	k.compilePreamble()
	traceLog("[jit] initializing")
	nt := LoadNeedTemplates()
	ps := LoadPatterns()
	ps.RebuildAllDescriptions()
	return &JIT{
		Knowledge:     k,
		Patterns:      ps,
		Scripts:       LoadScripts(),
		CodeSkills:    LoadCodeSkills(),
		TraceIndex:    LoadTraceIndex(),
		NeedTemplates: nt,
	}
}

// Plan analyzes a prompt and returns an execution plan.
// Checks tiers top-down: 3 → 2 → 1 → 0.
func (j *JIT) Plan(prompt string, auth *AuthMethod) *ExecutionPlan {
	plan := &ExecutionPlan{
		Tier: Tier0,
	}

	if script := j.Scripts.MatchScript(prompt); script != nil {
		if cmd, ok := scriptSingleAssertCommand(script); ok {
			if execNow, exclusive, fit, ic, src := classifyExecutionIntent(prompt, cmd); execNow && exclusive && fit && ic >= executionIntentThreshold {
				plan.Tier = Tier3
				plan.Script = script
				traceLog("[jit] tier 3: script %q (%d uses, %d failures; %s %.2f)", script.Name, script.Uses, script.Failures, src, ic)
				return plan
			}
			traceLog("[jit] script match skipped by intent gate: %q", script.Name)
		} else {
			plan.Tier = Tier3
			plan.Script = script
			traceLog("[jit] tier 3: script %q (%d uses, %d failures)", script.Name, script.Uses, script.Failures)
			return plan
		}
	}

	match := j.FindMatch(prompt)
	if match != nil && match.Pattern != nil && match.Confidence >= 0.8 {
		plan.Tier = Tier2
		plan.Pattern = match.Pattern
		plan.MatchParams = match.Params
		plan.MatchStrategy = match.Strategy
		plan.MatchConfidence = match.Confidence

		if j.patternIntent(match.Pattern.ID) == "run_checks" {
			if step, ok := stableAssertBashStep(match.Pattern); ok {
				cmd := scriptStepCommand(step)
				if execNow, exclusive, fit, ic, src := classifyExecutionIntent(prompt, cmd); execNow && exclusive && fit && ic >= executionIntentThreshold {
					plan.Tier = Tier3
					plan.Script = &Script{
						ID:       "adhoc_" + match.Pattern.ID,
						Name:     "adhoc: run_checks",
						Keywords: match.Pattern.Keywords,
						Steps:    []ScriptStep{step},
					}
					traceLog("[jit] tier 3: adhoc run_checks (pattern=%s via %s conf=%.2f intent=%s %.2f)", match.Pattern.ID, match.Strategy, match.Confidence, src, ic)
					return plan
				}
			}
		}

		if step, ok := singleStableAssertBashStep(match.Pattern); ok {
			cmd := scriptStepCommand(step)
			if execNow, exclusive, fit, ic, src := classifyExecutionIntent(prompt, cmd); execNow && exclusive && fit && ic >= executionIntentThreshold {
				plan.Tier = Tier3
				plan.Script = &Script{
					ID:       "adhoc_" + match.Pattern.ID,
					Name:     "adhoc: assert",
					Keywords: match.Pattern.Keywords,
					Steps:    []ScriptStep{step},
				}
				traceLog("[jit] tier 3: adhoc assert step (pattern=%s via %s conf=%.2f intent=%s %.2f)", match.Pattern.ID, match.Strategy, match.Confidence, src, ic)
				return plan
			}
		}

		if tmpl := j.NeedTemplates.FindByPattern(match.Pattern.ID); tmpl != nil && len(tmpl.Needs) > 0 {
			entity := extractEntity(prompt, tmpl)
			resolved := Resolve(tmpl.Needs, entity)
			if len(resolved) > 0 {
				plan.NeedContext = AssembleContext(resolved, prompt)
				plan.ResolvedNeeds = resolved
				traceLog("[jit] tier 2: pattern %q via %s (conf=%.2f), resolved %d needs for entity=%q",
					match.Pattern.ID, match.Strategy, match.Confidence, len(resolved), entity)
			} else {
				traceLog("[jit] tier 2: pattern %q via %s (conf=%.2f), no needs resolved",
					match.Pattern.ID, match.Strategy, match.Confidence)
			}
		} else {
			traceLog("[jit] tier 2: pattern %q via %s (conf=%.2f, occ=%d), no need template yet",
				match.Pattern.ID, match.Strategy, match.Confidence, match.Pattern.Occurrences)
		}
		return plan
	}

	if j.Knowledge != nil && j.Knowledge.Preamble != "" {
		plan.Tier = Tier1
		plan.Preamble = j.Knowledge.Preamble
		traceLog("[jit] tier 1: knowledge preamble (%d chars)", len(plan.Preamble))
	} else {
		if j.Knowledge == nil {
			traceLog("[jit] tier 1 skipped: no knowledge")
		} else if j.Knowledge.Preamble == "" {
			traceLog("[jit] tier 1 skipped: empty preamble")
		}
	}

	traceLog("[jit] tier %d: %s", plan.Tier, tierName(plan.Tier))
	return plan
}

func (j *JIT) patternIntent(patternID string) string {
	if j == nil || j.NeedTemplates == nil || patternID == "" {
		return ""
	}
	if tmpl := j.NeedTemplates.FindByPattern(patternID); tmpl != nil {
		return strings.TrimSpace(strings.ToLower(tmpl.Intent))
	}
	return ""
}

func singleStableAssertBashStep(p *Pattern) (ScriptStep, bool) {
	if p == nil || len(p.Ops) != 1 {
		return ScriptStep{}, false
	}
	op := p.Ops[0]
	if op.Tool != "bash" || op.Kind != OpAssert || op.Stability < 0.8 {
		return ScriptStep{}, false
	}
	argsJSON, _ := json.Marshal(op.StableArgs)
	return ScriptStep{
		Tool: "bash",
		Args: argsJSON,
		Kind: op.Kind,
	}, true
}

func stableAssertBashStep(p *Pattern) (ScriptStep, bool) {
	if p == nil || len(p.Ops) == 0 {
		return ScriptStep{}, false
	}
	seen := map[string]bool{}
	var canonical string
	for _, op := range p.Ops {
		if op.Tool != "bash" || op.Kind != OpAssert || op.Stability < 0.8 {
			continue
		}
		cmd := canonicalAssertCommand(op.StableArgs["command"])
		if cmd == "" {
			continue
		}
		seen[cmd] = true
		canonical = cmd
	}
	if len(seen) != 1 || canonical == "" {
		return ScriptStep{}, false
	}
	argsJSON, _ := json.Marshal(map[string]string{"command": canonical})
	return ScriptStep{
		Tool: "bash",
		Args: argsJSON,
		Kind: OpAssert,
	}, true
}

func canonicalAssertCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return ""
	}
	normalized := strings.ReplaceAll(cmd, "&&", ";")
	normalized = strings.ReplaceAll(normalized, "||", ";")
	parts := strings.Split(normalized, ";")
	last := ""
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			last = p
		}
	}
	return strings.TrimSpace(last)
}

func scriptStepCommand(step ScriptStep) string {
	var a struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(step.Args, &a)
	return strings.TrimSpace(a.Command)
}

func scriptSingleAssertCommand(script *Script) (string, bool) {
	if script == nil || len(script.Steps) != 1 {
		return "", false
	}
	step := script.Steps[0]
	if step.Tool != "bash" || step.Kind != OpAssert {
		return "", false
	}
	cmd := canonicalAssertCommand(scriptStepCommand(step))
	if cmd == "" {
		return "", false
	}
	return cmd, true
}

func classifyExecutionIntent(prompt, command string) (bool, bool, bool, float64, string) {
	key := executionIntentCacheKey(prompt, command)
	if key != "" {
		executionIntentCache.RLock()
		if d, ok := executionIntentCache.byKey[key]; ok {
			executionIntentCache.RUnlock()
			if time.Now().Before(d.ExpiresAt) {
				return d.ExecuteNow, d.Exclusive, d.CommandFit, d.Confidence, "intent_cache"
			}
			executionIntentCache.Lock()
			delete(executionIntentCache.byKey, key)
			executionIntentCache.Unlock()
		}
		executionIntentCache.RUnlock()
	}

	if execNow, exclusive, fit, conf, ok, reason := classifyExecutionIntentLogReg(prompt, command); ok {
		cacheExecutionIntent(key, execNow, exclusive, fit, conf)
		return execNow, exclusive, fit, conf, "logreg_intent"
	} else if traceJIT {
		traceLog("[jit] intent gate: logreg abstain (%s)", reason)
	}

	return false, false, false, 0, "none"
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func executionIntentCacheKey(prompt, command string) string {
	if strings.TrimSpace(prompt) == "" || strings.TrimSpace(command) == "" {
		return ""
	}
	pTrig, _ := normalizeTrigger(prompt)
	cmd := canonicalAssertCommand(command)
	if pTrig == "" || cmd == "" {
		return ""
	}
	return pTrig + "|" + strings.ToLower(cmd)
}

func cacheExecutionIntent(key string, execNow bool, exclusive bool, fit bool, conf float64) {
	if key == "" {
		return
	}
	if !(execNow && exclusive && fit && conf >= 0.8) {
		return
	}
	now := time.Now()
	ttl := 15 * time.Minute
	executionIntentCache.Lock()
	executionIntentCache.byKey[key] = executionIntentDecision{
		ExecuteNow: execNow,
		Exclusive:  exclusive,
		CommandFit: fit,
		Confidence: conf,
		ExpiresAt:  now.Add(ttl),
	}
	executionIntentCache.Unlock()
}

// Record learns from a completed interaction.
func (j *JIT) Record(prompt string, trace *IRTrace) {
	if trace == nil {
		return
	}

	j.Knowledge.LearnFromTrace(trace)
	j.Knowledge.Save()

	j.Patterns.LearnPattern(trace)

	sig := CanonicalSignature(trace.Ops)
	var traceEmb []float64
	if ollamaAvailable() {
		traceEmb = ollamaEmbed(trace.Trigger)
	}
	for _, p := range j.Patterns.Patterns {
		sigOverlap := 0.0
		if p.Signature != nil {
			sigOverlap = SignatureOverlap(sig, p.Signature)
		}
		embSim := 0.0
		if traceEmb != nil && len(p.Embedding) > 0 {
			embSim = cosineSimilarity(traceEmb, p.Embedding)
		}
		if sigOverlap >= 0.8 || embSim >= 0.80 {
			j.NeedTemplates.LearnFromTrace(trace, p)
			break
		}
	}
	j.NeedTemplates.Save()

	j.TraceIndex.Add(trace.Trigger, trace.Signature, trace.Ops)
	j.TraceIndex.Save()

	PromotePatterns(j.Patterns, j.Scripts)

	j.Patterns.Save()
	j.Scripts.Save()

	traceLog("[jit] recorded: %d patterns, %d scripts, %d file outlines, %d interactions, %d trace index entries, %d need templates",
		len(j.Patterns.Patterns), len(j.Scripts.Scripts),
		len(j.Knowledge.FileIndex), j.Knowledge.TotalInteractions,
		len(j.TraceIndex.Entries), len(j.NeedTemplates.Templates))
}

// RecordSpecSuccess notes that speculation worked (LLM used the pre-fetched data).
// Also counts as an occurrence so the pattern can be promoted to a script.
func (j *JIT) RecordSpecSuccess(pattern *Pattern) {
	pattern.Occurrences++
	pattern.Successes++
	PromotePatterns(j.Patterns, j.Scripts)
	j.Patterns.Save()
	j.Scripts.Save()
}

// RecordScriptFailure notes that a script failed (will deopt next time if rate > 20%).
func (j *JIT) RecordScriptFailure(script *Script) {
	j.Scripts.Save()
}

func (j *JIT) Stats() string {
	nCodeSkills := 0
	if j.CodeSkills != nil {
		nCodeSkills = len(j.CodeSkills.Skills)
	}
	return fmt.Sprintf("T1: %d decisions, %d files | T2: %d patterns, %d need templates | T3: %d scripts, %d code skills | %d interactions",
		len(j.Knowledge.Decisions), len(j.Knowledge.FileIndex),
		len(j.Patterns.Patterns), len(j.NeedTemplates.Templates),
		len(j.Scripts.Scripts), nCodeSkills,
		j.Knowledge.TotalInteractions)
}

func tierName(t Tier) string {
	switch t {
	case Tier0:
		return "cold (no optimization)"
	case Tier1:
		return "knowledge preamble"
	case Tier2:
		return "speculative execution"
	case Tier3:
		return "compiled skill/script"
	}
	return "unknown"
}

func runWithJIT(prompt string, auth *AuthMethod) *ResponseStats {
	start := time.Now()
	plan := jitEngine.Plan(prompt, auth)
	event := JITEvent{
		Timestamp:       time.Now(),
		Mode:            "run",
		Prompt:          prompt,
		EffectivePrompt: prompt,
		PlannedTier:     int(plan.Tier),
		PlannedTierName: tierName(plan.Tier),
		MatchStrategy:   plan.MatchStrategy,
		MatchConfidence: plan.MatchConfidence,
	}
	if plan.Pattern != nil {
		event.PatternID = plan.Pattern.ID
	}
	if plan.Script != nil {
		event.ScriptName = plan.Script.Name
	}

	switch plan.Tier {
	case Tier3:
		recorder := NewTraceRecorder(prompt)
		effects, err := ExecuteScript(plan.Script, recorder)
		if err != nil {
			traceLog("[jit] deopt tier 3 → tier 0: %v", err)
			jitEngine.RecordScriptFailure(plan.Script)
			specContext := buildScriptDeoptContext(plan.Script, effects, err)
			messages := []Message{{Role: "user", Content: prompt}}
			event.ScriptDeopt = true
			event.InjectedContext = "deopt"
			event.InjectedPreview = specContext
			stats, runErr := handleResponse(&messages, auth, prompt, plan.Preamble, &specContext, event.ToMeta())
			if runErr != nil {
				event.Error = runErr.Error()
				fmt.Fprintf(os.Stderr, "error: %v\n", runErr)
				appendJITEvent(event)
				return &ResponseStats{ElapsedMs: float64(time.Since(start).Milliseconds())}
			}
			event.APICalls = stats.APICalls
			event.LLMToolCalls = stats.LLMToolCalls
			event.InputTokens = stats.InputTokens
			event.OutputTokens = stats.OutputTokens
			event.ElapsedMs = stats.ElapsedMs
			appendJITEvent(event)
			return stats
		}
		event.ElapsedMs = float64(time.Since(start).Milliseconds())
		if err := onlineTrainIntentFromTurn(prompt, true); err != nil && traceJIT {
			traceLog("[jit] online intent train error: %v", err)
		}
		appendJITEvent(event)
		return &ResponseStats{ElapsedMs: event.ElapsedMs}

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
			recorder := NewTraceRecorder(prompt)
			specResults := Speculate(prompt, plan.Pattern, recorder)
			event.SpeculatedOps = len(specResults)
			specContext = PackSpecResults(specResults)
			if event.SpeculatedOps > 0 {
				event.InjectedContext = "spec"
				event.InjectedPreview = specContext
			}
		}
		messages := []Message{{Role: "user", Content: prompt}}
		stats, err := handleResponse(&messages, auth, prompt, plan.Preamble, &specContext, event.ToMeta())
		if err != nil {
			event.Error = err.Error()
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			appendJITEvent(event)
			return &ResponseStats{ElapsedMs: float64(time.Since(start).Milliseconds())}
		}
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
		appendJITEvent(event)
		return stats

	default:
		messages := []Message{{Role: "user", Content: prompt}}
		stats, err := handleResponse(&messages, auth, prompt, plan.Preamble, nil, event.ToMeta())
		if err != nil {
			event.Error = err.Error()
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			appendJITEvent(event)
			return &ResponseStats{ElapsedMs: float64(time.Since(start).Milliseconds())}
		}
		event.APICalls = stats.APICalls
		event.LLMToolCalls = stats.LLMToolCalls
		event.InputTokens = stats.InputTokens
		event.OutputTokens = stats.OutputTokens
		event.ElapsedMs = stats.ElapsedMs
		appendJITEvent(event)
		return stats
	}
}

func buildScriptDeoptContext(script *Script, effects []Effect, scriptErr error) string {
	const maxOutputChars = 8000
	var b strings.Builder

	b.WriteString("A deterministic script already ran for this request before this response.\n")
	if script != nil && script.Name != "" {
		b.WriteString("Script: ")
		b.WriteString(script.Name)
		b.WriteByte('\n')
	}
	if scriptErr != nil {
		b.WriteString("Script error: ")
		b.WriteString(scriptErr.Error())
		b.WriteByte('\n')
	}
	b.WriteString("Do not rerun the same command unless you must gather new information.\n\n")

	for i, e := range effects {
		fmt.Fprintf(&b, "Step %d: %s\n", i+1, e.Tool)
		if e.Tool == "bash" {
			cmd := strings.TrimSpace(extractBashCommand(e.Input))
			if cmd != "" {
				b.WriteString("Command: ")
				b.WriteString(cmd)
				b.WriteByte('\n')
			}
		}
		if e.IsError {
			b.WriteString("Result: FAILED\n")
		} else {
			b.WriteString("Result: OK\n")
		}
		out := strings.TrimSpace(e.Output)
		if out != "" {
			b.WriteString("Output:\n")
			b.WriteString(clipForContext(out, maxOutputChars))
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	return b.String()
}

func clipForContext(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max / 2
	tail := max - head
	if tail < 0 {
		tail = 0
	}
	return s[:head] + "\n... [truncated] ...\n" + s[len(s)-tail:]
}

// resolveAnaphoraInput rewrites short anaphoric prompts ("run it again", "rerun")

// CodeSkill is a reusable TypeScript snippet that runs via the code tool.
type CodeSkill struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Keywords    []string  `json:"keywords"`
	Code        string    `json:"code"`
	Uses        int       `json:"uses,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	Created     time.Time `json:"created,omitempty"`
}

// CodeSkillStore holds all code skills.
type CodeSkillStore struct {
	Skills []*CodeSkill `json:"skills"`
}

// LoadCodeSkills loads code skills from .oc/code_skills/
func LoadCodeSkills() *CodeSkillStore {
	base, err := ocBaseDir()
	if err != nil {
		return &CodeSkillStore{}
	}
	dir := filepath.Join(base, "code_skills")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &CodeSkillStore{}
	}
	var skills []*CodeSkill
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s CodeSkill
		if json.Unmarshal(data, &s) == nil && s.Name != "" && s.Code != "" {
			skills = append(skills, &s)
		}
	}
	return &CodeSkillStore{Skills: skills}
}

// Save persists all code skills to disk.
func (cs *CodeSkillStore) Save() error {
	base, err := ocBaseDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "code_skills")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, s := range cs.Skills {
		if s.Name == "" || s.Code == "" {
			continue
		}
		data, _ := json.MarshalIndent(s, "", "  ")
		filename := sanitizeFilename(s.Name) + ".json"
		_ = os.WriteFile(filepath.Join(dir, filename), data, 0o644)
	}
	return nil
}

// Add adds or updates a code skill.
func (cs *CodeSkillStore) Add(skill *CodeSkill) {
	for i, s := range cs.Skills {
		if s.Name == skill.Name {
			cs.Skills[i] = skill
			return
		}
	}
	cs.Skills = append(cs.Skills, skill)
}

// FindByName returns a skill by exact name match.
func (cs *CodeSkillStore) FindByName(name string) *CodeSkill {
	name = strings.TrimSpace(strings.ToLower(name))
	for _, s := range cs.Skills {
		if strings.ToLower(s.Name) == name {
			return s
		}
	}
	return nil
}

func sanitizeFilename(name string) string {
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "_")
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	return name
}

// Script is a fully compiled deterministic sequence (Tier 3).
// No LLM needed — just execute the steps.
type Script struct {
	ID       string       `json:"id"`
	Name     string       `json:"name"`
	Keywords []string     `json:"keywords"`
	Steps    []ScriptStep `json:"steps"`
	Uses     int          `json:"uses"`
	Failures int          `json:"failures"`
	Created  time.Time    `json:"created"`
}

type ScriptStep struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
	Kind OpKind          `json:"kind"`
}

type ScriptStore struct {
	Scripts []*Script `json:"scripts"`
}

func LoadScripts() *ScriptStore {
	base, err := ocBaseDir()
	if err != nil {
		return &ScriptStore{}
	}
	dir := filepath.Join(base, "scripts")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &ScriptStore{}
	}
	var scripts []*Script
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var s Script
		if json.Unmarshal(data, &s) == nil {
			scripts = append(scripts, &s)
		}
	}
	return &ScriptStore{Scripts: scripts}
}

func (ss *ScriptStore) Save() error {
	base, err := ocBaseDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(base, "scripts")
	os.MkdirAll(dir, 0o755)
	for _, s := range ss.Scripts {
		data, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			continue
		}
		os.WriteFile(filepath.Join(dir, s.ID+".json"), data, 0o644)
	}
	return nil
}

// MatchScript finds a script matching the prompt.
// High threshold: 0.9 keyword overlap, <20% failure rate.
func (ss *ScriptStore) MatchScript(prompt string) *Script {
	words := filterWords(strings.Fields(strings.ToLower(prompt)))
	for _, s := range ss.Scripts {
		if s.Uses+s.Failures > 0 {
			failRate := float64(s.Failures) / float64(s.Uses+s.Failures)
			if failRate > 0.2 {
				continue
			}
		}
		overlap := wordOverlap(words, s.Keywords)
		if overlap >= 0.9 {
			return s
		}
	}
	return nil
}

// ExecuteScript runs a compiled script. Returns effects and any error.
func ExecuteScript(script *Script, recorder *TraceRecorder) ([]Effect, error) {
	var effects []Effect
	for i, step := range script.Steps {
		fmt.Println(dim(fmt.Sprintf("script [%d/%d]: %s", i+1, len(script.Steps), formatToolCall(step.Tool, step.Args))))

		start := time.Now()
		result := executeTool(step.Tool, step.Args)
		elapsed := time.Since(start)

		effects = append(effects, Effect{
			Tool:     step.Tool,
			Input:    step.Args,
			Output:   result.Content,
			IsError:  result.IsError,
			Duration: elapsed.Milliseconds(),
		})

		if recorder != nil {
			recorder.RecordEffect(step.Tool, step.Args, result, elapsed)
		}

		if result.IsError {
			script.Failures++
			return effects, fmt.Errorf("script step %d failed: %s", i+1, result.Content)
		}

		if step.Kind == OpRead || step.Kind == OpQuery {
			content := result.Content
			if len(content) > 3000 {
				content = content[:3000] + "\n... (truncated)"
			}
			fmt.Println(dim(content))
		}
	}
	script.Uses++
	return effects, nil
}

// PromotePatterns checks if any patterns should be promoted to scripts.
// Criteria: 3+ occurrences, >80% success rate, all ops have concrete args.
func PromotePatterns(ps *PatternStore, ss *ScriptStore) {
	for _, p := range ps.Patterns {
		if p.Occurrences < 3 {
			continue
		}
		rate := float64(p.Successes) / float64(p.Occurrences)
		if rate < 0.8 {
			continue
		}
		alreadyPromoted := false
		for _, s := range ss.Scripts {
			if s.ID == p.ID {
				alreadyPromoted = true
				break
			}
		}
		if alreadyPromoted {
			continue
		}
		allConcrete := true
		var steps []ScriptStep
		for _, op := range p.Ops {
			if op.Stability < 0.8 {
				allConcrete = false
				break
			}
			argsJSON, _ := json.Marshal(op.StableArgs)
			steps = append(steps, ScriptStep{Tool: op.Tool, Args: json.RawMessage(argsJSON), Kind: op.Kind})
		}
		if !allConcrete || len(steps) == 0 {
			continue
		}
		ss.Scripts = append(ss.Scripts, &Script{
			ID:       p.ID,
			Name:     strings.Join(p.Keywords, " "),
			Keywords: p.Keywords,
			Steps:    steps,
			Created:  time.Now(),
		})
		traceLog("[jit] promoted pattern %s to script (%d occurrences, %.0f%% success)",
			p.ID, p.Occurrences, rate*100)
	}
}

// OpKind classifies what an IR operation does.
type OpKind string

const (
	OpRead   OpKind = "read"
	OpWrite  OpKind = "write"
	OpExec   OpKind = "exec"
	OpQuery  OpKind = "query"  // list_files, grep
	OpAssert OpKind = "assert" // test/lint commands
)

// IRop is one normalized tool invocation.
type IRop struct {
	Index     int             `json:"index"`
	Kind      OpKind          `json:"kind"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	DependsOn []int           `json:"depends_on"`
	ReadSet   []string        `json:"read_set"`
	WriteSet  []string        `json:"write_set"`
	Duration  time.Duration   `json:"duration"`
	Output    string          `json:"output"`
	IsError   bool            `json:"is_error"`
}

// IRTrace is a compiled trace — the normalized form of a workflow.
type IRTrace struct {
	ID         string     `json:"id"`
	Trigger    string     `json:"trigger"`
	TriggerID  string     `json:"trigger_id"`
	RawTrigger string     `json:"raw_trigger,omitempty"` // original user prompt (unnormalized)
	Ops        []IRop     `json:"ops"`
	Signature  string     `json:"signature"`
	Created    time.Time  `json:"created"`
	Stats      TraceStats `json:"stats"`
}

// CompileIR converts raw trace effects into IR.
func CompileIR(trigger string, effects []Effect) *IRTrace {
	if len(effects) == 0 {
		return nil
	}

	trig, trigID := normalizeTrigger(trigger)

	ops := make([]IRop, len(effects))
	for i, e := range effects {
		ops[i] = IRop{
			Index:    i,
			Kind:     ClassifyOp(e.Tool, e.Input),
			Tool:     e.Tool,
			Args:     e.Input,
			ReadSet:  extractReadSet(e.Tool, e.Input),
			WriteSet: extractWriteSet(e.Tool, e.Input),
			Duration: time.Duration(e.Duration) * time.Millisecond,
			Output:   e.Output,
			IsError:  e.IsError,
		}
	}

	ops = ComputeDeps(ops)

	return &IRTrace{
		ID:        trigID,
		Trigger:   trig,
		TriggerID: trigID,
		Ops:       ops,
		Signature: ComputeSignature(ops),
		Created:   time.Now(),
	}
}

// ClassifyOp determines OpKind from tool name + args.
func ClassifyOp(tool string, args json.RawMessage) OpKind {
	switch tool {
	case "read_file", "read_files":
		return OpRead
	case "write_file", "write_files":
		return OpWrite
	case "list_files", "grep", "find_symbol":
		return OpQuery
	case "bash":
		return classifyBashOp(args)
	default:
		return OpExec
	}
}

func classifyBashOp(args json.RawMessage) OpKind {
	var a struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &a) != nil {
		return OpExec
	}
	cmd := strings.ToLower(a.Command)

	for _, kw := range []string{"test", "pytest", "go test", "cargo test", "npm test", "lint", "check", "vet"} {
		if strings.Contains(cmd, kw) {
			return OpAssert
		}
	}

	for _, kw := range []string{"grep", "find", "ls", "cat", "head", "tail", "wc", "tree", "rg", "ag"} {
		fields := strings.Fields(cmd)
		if len(fields) > 0 && (fields[0] == kw || strings.HasSuffix(fields[0], "/"+kw)) {
			return OpQuery
		}
	}

	for _, kw := range []string{"mkdir", "rm", "mv", "cp", "touch", "chmod"} {
		fields := strings.Fields(cmd)
		if len(fields) > 0 && (fields[0] == kw || strings.HasSuffix(fields[0], "/"+kw)) {
			return OpWrite
		}
	}

	return OpExec
}

// ComputeDeps analyzes read/write sets to find data dependencies.
func ComputeDeps(ops []IRop) []IRop {
	lastWriter := make(map[string]int)

	for i := range ops {
		var deps []int
		seen := make(map[int]bool)

		for _, path := range ops[i].ReadSet {
			if writerIdx, ok := lastWriter[path]; ok && !seen[writerIdx] {
				deps = append(deps, writerIdx)
				seen[writerIdx] = true
			}
		}

		ops[i].DependsOn = deps

		for _, path := range ops[i].WriteSet {
			lastWriter[path] = i
		}
	}

	return ops
}

// ComputeSignature produces a structural hash: "exec|write|write|exec" style.
func ComputeSignature(ops []IRop) string {
	parts := make([]string, len(ops))
	for i, op := range ops {
		parts[i] = string(op.Kind) + ":" + op.Tool
	}
	raw := strings.Join(parts, "|")
	h := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", h)[:16]
}

// extractReadSet gets the files/dirs this operation reads.
func extractReadSet(tool string, args json.RawMessage) []string {
	var a map[string]string
	json.Unmarshal(args, &a)

	switch tool {
	case "read_file":
		if p := a["path"]; p != "" {
			return []string{p}
		}
	case "read_files":
		var aa struct {
			Paths []string `json:"paths"`
		}
		if json.Unmarshal(args, &aa) == nil && len(aa.Paths) > 0 {
			out := make([]string, 0, len(aa.Paths))
			for _, p := range aa.Paths {
				if p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	case "list_files":
		if p := a["path"]; p != "" {
			return []string{p}
		}
	case "grep":
		if p := a["path"]; p != "" {
			return []string{p}
		}
		return []string{"."}
	case "find_symbol":
		if p := a["path"]; p != "" {
			return []string{p}
		}
		return []string{"."}
	case "bash":
		return extractBashReadPaths(a["command"])
	}
	return nil
}

// extractWriteSet gets the files/dirs this operation writes.
func extractWriteSet(tool string, args json.RawMessage) []string {
	var a map[string]string
	json.Unmarshal(args, &a)

	switch tool {
	case "write_file":
		if p := a["path"]; p != "" {
			return []string{p}
		}
	case "write_files":
		var aa struct {
			Files []struct {
				Path string `json:"path"`
			} `json:"files"`
		}
		if json.Unmarshal(args, &aa) == nil && len(aa.Files) > 0 {
			out := make([]string, 0, len(aa.Files))
			for _, f := range aa.Files {
				if f.Path != "" {
					out = append(out, f.Path)
				}
			}
			return out
		}
	case "bash":
		return extractBashWritePaths(a["command"])
	}
	return nil
}

// extractBashReadPaths tries to extract file paths from common read commands.
func extractBashReadPaths(cmd string) []string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return nil
	}
	base := fields[0]
	switch base {
	case "cat", "head", "tail", "less", "wc":
		var paths []string
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				paths = append(paths, f)
			}
		}
		return paths
	}
	return nil
}

// CanonicalSignature returns an order-independent bag of "kind:tool" counts.
// Used for structural pattern merging (not user-facing matching).
// Counts are capped at 3 to normalize traces where the number of read_file
// calls varies based on grep results (e.g., 2 vs 8 files matching).
func CanonicalSignature(ops []IRop) map[string]int {
	bag := make(map[string]int)
	for _, op := range ops {
		key := string(op.Kind) + ":" + op.Tool
		bag[key]++
	}
	for k, v := range bag {
		if v > 3 {
			bag[k] = 3
		}
	}
	return bag
}

// SignatureOverlap returns Jaccard similarity on multisets (bags).
// For each key, the intersection count is min(a[k], b[k]) and union is max(a[k], b[k]).
func SignatureOverlap(a, b map[string]int) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	keys := make(map[string]bool)
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var intersection, union int
	for k := range keys {
		av, bv := a[k], b[k]
		if av < bv {
			intersection += av
			union += bv
		} else {
			intersection += bv
			union += av
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// extractBashWritePaths tries to extract file paths from common write commands.
func extractBashWritePaths(cmd string) []string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return nil
	}
	base := fields[0]
	switch base {
	case "mkdir", "touch":
		var paths []string
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				paths = append(paths, f)
			}
		}
		return paths
	case "cp", "mv":
		nonFlags := []string{}
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				nonFlags = append(nonFlags, f)
			}
		}
		if len(nonFlags) > 0 {
			return []string{nonFlags[len(nonFlags)-1]}
		}
	case "rm":
		var paths []string
		for _, f := range fields[1:] {
			if !strings.HasPrefix(f, "-") {
				paths = append(paths, f)
			}
		}
		return paths
	case "cargo":
		if len(fields) >= 3 && (fields[1] == "new" || fields[1] == "init") {
			return []string{fields[2]}
		}
	case "go":
		if len(fields) >= 3 && fields[1] == "mod" && fields[2] == "init" {
			return []string{"go.mod"}
		}
		if len(fields) >= 2 && fields[1] == "build" {
			return []string{"."} // approximate
		}
	case "npm", "yarn", "pnpm":
		if len(fields) >= 2 && fields[1] == "init" {
			return []string{"package.json"}
		}
	case "pip", "pip3":
	case "python3", "python":
		if len(fields) >= 4 && fields[1] == "-m" && fields[2] == "venv" {
			return []string{fields[3]}
		}
	}
	return nil
}

type JITEvent struct {
	Timestamp       time.Time `json:"ts"`
	Mode            string    `json:"mode,omitempty"` // "interactive" | "run"
	SessionID       string    `json:"session_id,omitempty"`
	Prompt          string    `json:"prompt,omitempty"`
	EffectivePrompt string    `json:"effective_prompt,omitempty"`
	AnaphoraFrom    string    `json:"anaphora_from,omitempty"`
	AnaphoraTo      string    `json:"anaphora_to,omitempty"`
	PlannedTier     int       `json:"planned_tier"`
	PlannedTierName string    `json:"planned_tier_name,omitempty"`
	PatternID       string    `json:"pattern_id,omitempty"`
	ScriptName      string    `json:"script_name,omitempty"`
	MatchStrategy   string    `json:"match_strategy,omitempty"`
	MatchConfidence float64   `json:"match_confidence,omitempty"`
	UsedNeedContext bool      `json:"used_need_context,omitempty"`
	ResolvedNeeds   int       `json:"resolved_needs,omitempty"`
	SpeculatedOps   int       `json:"speculated_ops,omitempty"`
	ScriptDeopt     bool      `json:"script_deopt,omitempty"`
	InjectedContext string    `json:"injected_context,omitempty"` // "spec", "needctx", "deopt", ""
	InjectedPreview string    `json:"injected_preview,omitempty"` // short preview for debugging
	APICalls        int       `json:"api_calls,omitempty"`
	LLMToolCalls    int       `json:"llm_tool_calls,omitempty"`
	InputTokens     int       `json:"input_tokens,omitempty"`
	OutputTokens    int       `json:"output_tokens,omitempty"`
	ElapsedMs       float64   `json:"elapsed_ms,omitempty"`
	Error           string    `json:"error,omitempty"`
}

func (e JITEvent) ToMeta() *JITMeta {
	return &JITMeta{
		PlannedTier:     e.PlannedTier,
		PlannedTierName: e.PlannedTierName,
		PatternID:       e.PatternID,
		ScriptName:      e.ScriptName,
		MatchStrategy:   e.MatchStrategy,
		MatchConfidence: e.MatchConfidence,
		UsedNeedContext: e.UsedNeedContext,
		ResolvedNeeds:   e.ResolvedNeeds,
		SpeculatedOps:   e.SpeculatedOps,
		ScriptDeopt:     e.ScriptDeopt,
	}
}

func appendJITEvent(e JITEvent) {
	base, err := ocBaseDir()
	if err != nil {
		return
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return
	}
	path := filepath.Join(base, "jit-events.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}
