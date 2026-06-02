# Agents Council Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use ultraship:subagent-driven-development (recommended) or ultraship:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `--council` mode that fans two LLM reviewers out in parallel, then runs a judge to arbitrate, deduplicate, and attribute findings.

**Architecture:** A `councilProvider` struct implements the existing `ModelProvider` interface, encapsulating all multi-provider logic. `runner.go` constructs a `councilProvider` instead of a single provider when `opts.CouncilEnabled` is true — the rest of the pipeline is unchanged. Config fields drive reviewer-2 and judge provider/model selection; the CLI flag is the only user-facing change.

> **Design note:** The spec describes a `Council *CouncilConfig` field on `RunOptions`. This plan uses `CouncilEnabled bool` instead — a deliberate simplification. All four provider/model fields already live in `ReviewConfig` (loaded inside `Run()`), so there is no need to duplicate them on `RunOptions`. `CouncilConfig` still exists as an internal struct inside `council.go` but is never exposed on `RunOptions`.

**Tech Stack:** Go, `internal/review` package, existing `ModelProvider` / `ProviderFactory` patterns, `encoding/json`, `sync`

---

## Risk Register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Judge outputs malformed JSON | Medium | Medium | `ParseFindings` already has a salvage path; council falls back to reviewer-1 on total parse failure |
| Codex provider not registered at runtime | Medium | Low | Fallback chain: codex → primary provider; error only when `council_provider` is explicitly set to an unknown name |
| Council doubles token cost unexpectedly | Low | Medium | All three calls appear in the usage table with distinct `council-*` phase labels |

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `internal/review/findings.go` | Modify | Add `Sources []string` to `Finding` |
| `internal/review/findings_test.go` | Modify | Test `Sources` round-trips through `ParseFindings` |
| `internal/review/config.go` | Modify | Add 4 council fields to `ReviewConfig`; add validation |
| `internal/review/config_test.go` | Modify | Test council config validation |
| `internal/review/council.go` | Create | `CouncilConfig`, `councilProvider`, `BuildJudgePrompt` |
| `internal/review/council_test.go` | Create | Unit tests for `BuildJudgePrompt` and `councilProvider.Run()` |
| `internal/review/runner.go` | Modify | Add `CouncilEnabled bool` to `RunOptions`; construct `councilProvider` when enabled |
| `cmd/review/cli/review.go` | Modify | Add `--council` boolean flag; set `opts.CouncilEnabled` |
| `internal/review/formatter.go` | Modify | Render `Sources` attribution tag in terminal and markdown output |
| `internal/review/formatter_test.go` | Modify | Test attribution tag rendering |
| `docs/review-flow.md` | Modify | Document the council path |

**Task dependency order:** Task 1 → Task 2 → Task 3 → Task 4 → Task 5 → Task 6 → Task 7. Each task builds on the previous.

---

## Task 1: Add `Sources` to `Finding` struct

**Files:**
- Modify: `internal/review/findings.go:12-23`
- Modify: `internal/review/findings_test.go`

- [ ] **Step 1: Write failing test**

Add to `internal/review/findings_test.go`:

```go
func TestParseFindingsWithSources(t *testing.T) {
	output := "```json\n[\n  {\n    \"id\": \"test\",\n    \"file\": \"main.go\",\n    \"line\": 1,\n    \"severity\": \"warning\",\n    \"title\": \"Test\",\n    \"description\": \"Desc\",\n    \"fix_ref\": \"1-0\",\n    \"sources\": [\"reviewer-1\", \"reviewer-2\"]\n  }\n]\n```\n"

	findings, err := ParseFindings(output)
	if err != nil {
		t.Fatalf("ParseFindings() error: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if len(findings[0].Sources) != 2 {
		t.Fatalf("expected 2 sources, got %v", findings[0].Sources)
	}
	if findings[0].Sources[0] != "reviewer-1" || findings[0].Sources[1] != "reviewer-2" {
		t.Errorf("unexpected sources: %v", findings[0].Sources)
	}
}

func TestParseFindingsSourcesOmitted(t *testing.T) {
	// Findings without sources (single-reviewer mode) must still parse cleanly.
	output := "```json\n[\n  {\n    \"id\": \"test\",\n    \"file\": \"main.go\",\n    \"line\": 1,\n    \"severity\": \"warning\",\n    \"title\": \"Test\",\n    \"description\": \"Desc\",\n    \"fix_ref\": \"1-0\"\n  }\n]\n```\n"

	findings, err := ParseFindings(output)
	if err != nil {
		t.Fatalf("ParseFindings() error: %v", err)
	}
	if findings[0].Sources != nil {
		t.Errorf("expected nil sources for non-council finding, got %v", findings[0].Sources)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd /Users/anderson/Documents/GitHub/playground/codecanary
go test ./internal/review/ -run TestParseFindingsWithSources -v
```

Expected: FAIL — `Finding` has no `Sources` field.

- [ ] **Step 3: Add `Sources` to `Finding`**

In `internal/review/findings.go`, add `Sources []string` after `Status`:

```go
type Finding struct {
	ID          string   `json:"id"`
	File        string   `json:"file"`
	Line        int      `json:"line"`
	Severity    string   `json:"severity"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Suggestion  string   `json:"suggestion,omitempty"`
	FixRef      string   `json:"fix_ref"`
	Actionable  *bool    `json:"actionable,omitempty"`
	Status      string   `json:"status,omitempty"`
	Sources     []string `json:"sources,omitempty"`
}
```

- [ ] **Step 4: Run all review tests**

```bash
go test ./internal/review/ -v 2>&1 | tail -20
```

Expected: all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/review/findings.go internal/review/findings_test.go
git commit -m "feat(council): add Sources field to Finding for reviewer attribution"
```

---

## Task 2: Add council config fields to `ReviewConfig`

**Files:**
- Modify: `internal/review/config.go:21-39` (struct) and `174-244` (Validate)
- Modify: `internal/review/config_test.go`

- [ ] **Step 1: Write failing validation test**

Find the config test file and add:

```go
func TestReviewConfigCouncilValidation(t *testing.T) {
	base := func() *ReviewConfig {
		return &ReviewConfig{
			Version:     1,
			Provider:    "anthropic",
			ReviewModel: "claude-opus-4-8",
			TriageModel: "claude-haiku-4-5",
		}
	}

	t.Run("unset council fields are valid", func(t *testing.T) {
		c := base()
		if err := c.Validate(); err != nil {
			t.Errorf("unexpected error: %v", err)
		}
	})

	t.Run("unknown council_provider is invalid", func(t *testing.T) {
		c := base()
		c.CouncilProvider = "nonexistent-provider"
		err := c.Validate()
		if err == nil {
			t.Fatal("expected error for unknown council_provider")
		}
		if !strings.Contains(err.Error(), "council_provider") {
			t.Errorf("error should mention council_provider: %v", err)
		}
	})
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/review/ -run TestReviewConfigCouncilValidation -v
```

Expected: FAIL — `CouncilProvider` field does not exist.

- [ ] **Step 3: Add council fields to `ReviewConfig`**

In `internal/review/config.go`, add after `ClaudePath string`:

```go
// Council mode — opt-in via --council flag. All fields are optional.
CouncilProvider      string `yaml:"council_provider"`       // provider for 2nd reviewer
CouncilModel         string `yaml:"council_model"`          // model for 2nd reviewer
CouncilJudgeProvider string `yaml:"council_judge_provider"` // provider for judge (default: anthropic)
CouncilJudgeModel    string `yaml:"council_judge_model"`    // model for judge (default: claude-opus-4-8)
```

- [ ] **Step 4: Add validation in `Validate()`**

In `internal/review/config.go`, at the end of `Validate()` before `return nil`, add:

```go
if c.CouncilProvider != "" {
    if _, ok := providers[c.CouncilProvider]; !ok {
        return fmt.Errorf("council_provider %q is not registered (valid: %s)", c.CouncilProvider, strings.Join(providerNames(), ", "))
    }
}
if c.CouncilJudgeProvider != "" {
    if _, ok := providers[c.CouncilJudgeProvider]; !ok {
        return fmt.Errorf("council_judge_provider %q is not registered (valid: %s)", c.CouncilJudgeProvider, strings.Join(providerNames(), ", "))
    }
}
```

- [ ] **Step 5: Run all tests**

```bash
go test ./internal/review/ -v 2>&1 | tail -20
```

Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/review/config.go internal/review/config_test.go
git commit -m "feat(council): add council provider/model config fields"
```

---

## Task 3: Create `council.go`

This is the core of the feature. The `councilProvider` implements `ModelProvider` by running two reviewers in parallel and having a judge arbitrate.

**Files:**
- Create: `internal/review/council.go`
- Create: `internal/review/council_test.go`

**Depends on:** Task 1 (Sources field) and Task 2 (config fields).

- [ ] **Step 1: Write failing tests in `council_test.go`**

```go
package review

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// mockProvider is a test double for ModelProvider.
type mockProvider struct {
	text string
	err  error
}

func (m *mockProvider) Run(_ context.Context, _ string, _ RunOpts) (*providerResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &providerResult{Text: m.text, Usage: CallUsage{Model: "mock"}}, nil
}

func councilFindingsJSON(findings []Finding) string {
	data, _ := marshalFindingsJSON(findings)
	return "```json\n" + string(data) + "\n```\n"
}

func TestBuildJudgePrompt(t *testing.T) {
	r1 := []Finding{{ID: "f1", File: "a.go", Line: 1, Severity: "bug", Title: "T1", Description: "D1", FixRef: "1-0"}}
	r2 := []Finding{{ID: "f2", File: "b.go", Line: 2, Severity: "warning", Title: "T2", Description: "D2", FixRef: "2-0"}}

	prompt := BuildJudgePrompt(r1, r2)

	if !strings.Contains(prompt, "REVIEWER 1 FINDINGS") {
		t.Error("prompt should contain REVIEWER 1 FINDINGS")
	}
	if !strings.Contains(prompt, "REVIEWER 2 FINDINGS") {
		t.Error("prompt should contain REVIEWER 2 FINDINGS")
	}
	if !strings.Contains(prompt, `"f1"`) {
		t.Error("prompt should include reviewer-1 finding IDs")
	}
	if !strings.Contains(prompt, `"f2"`) {
		t.Error("prompt should include reviewer-2 finding IDs")
	}
	if !strings.Contains(prompt, "sources") {
		t.Error("prompt should instruct judge to set sources")
	}
}

func TestBuildJudgePromptEmptyReviewer(t *testing.T) {
	// One reviewer returned no findings — prompt should still be valid.
	r1 := []Finding{}
	r2 := []Finding{{ID: "f2", File: "b.go", Line: 1, Severity: "bug", Title: "T", Description: "D", FixRef: "1-0"}}
	prompt := BuildJudgePrompt(r1, r2)
	if !strings.Contains(prompt, "REVIEWER 2 FINDINGS") {
		t.Error("prompt should contain reviewer 2 findings section")
	}
}

func TestCouncilProviderBothSucceed(t *testing.T) {
	r1Out := councilFindingsJSON([]Finding{
		{ID: "agreed", File: "a.go", Line: 1, Severity: "bug", Title: "T", Description: "D", FixRef: "1-0"},
	})
	r2Out := councilFindingsJSON([]Finding{
		{ID: "agreed", File: "a.go", Line: 1, Severity: "bug", Title: "T", Description: "D", FixRef: "1-0"},
	})
	judgeOut := councilFindingsJSON([]Finding{
		{ID: "agreed", File: "a.go", Line: 1, Severity: "bug", Title: "T", Description: "D", FixRef: "1-0", Sources: []string{"reviewer-1", "reviewer-2"}},
	})

	cp := &councilProvider{
		reviewer1: &mockProvider{text: r1Out},
		reviewer2: &mockProvider{text: r2Out},
		judge:     &mockProvider{text: judgeOut},
	}

	result, err := cp.Run(context.Background(), "prompt", RunOpts{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	findings, err := ParseFindings(result.Text)
	if err != nil {
		t.Fatalf("could not parse judge output: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Sources[0] != "reviewer-1" {
		t.Errorf("expected reviewer-1 source, got %v", findings[0].Sources)
	}
}

func TestCouncilProviderReviewer1Fails(t *testing.T) {
	r2Out := councilFindingsJSON([]Finding{
		{ID: "solo", File: "a.go", Line: 1, Severity: "warning", Title: "T", Description: "D", FixRef: "1-0"},
	})
	judgeOut := councilFindingsJSON([]Finding{
		{ID: "solo", File: "a.go", Line: 1, Severity: "warning", Title: "T", Description: "D", FixRef: "1-0", Sources: []string{"reviewer-2"}},
	})

	cp := &councilProvider{
		reviewer1: &mockProvider{err: errors.New("timeout")},
		reviewer2: &mockProvider{text: r2Out},
		judge:     &mockProvider{text: judgeOut},
	}

	result, err := cp.Run(context.Background(), "prompt", RunOpts{})
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	if result == nil || result.Text == "" {
		t.Fatal("expected non-empty result")
	}
}

func TestCouncilProviderBothFail(t *testing.T) {
	cp := &councilProvider{
		reviewer1: &mockProvider{err: errors.New("err1")},
		reviewer2: &mockProvider{err: errors.New("err2")},
		judge:     &mockProvider{text: ""},
	}

	_, err := cp.Run(context.Background(), "prompt", RunOpts{})
	if err == nil {
		t.Fatal("expected error when both reviewers fail")
	}
}

func TestCouncilProviderJudgeFails(t *testing.T) {
	r1Findings := []Finding{
		{ID: "f1", File: "a.go", Line: 1, Severity: "bug", Title: "T", Description: "D", FixRef: "1-0"},
	}
	r1Out := councilFindingsJSON(r1Findings)
	r2Out := councilFindingsJSON([]Finding{})

	cp := &councilProvider{
		reviewer1: &mockProvider{text: r1Out},
		reviewer2: &mockProvider{text: r2Out},
		judge:     &mockProvider{err: errors.New("judge failure")},
	}

	result, err := cp.Run(context.Background(), "prompt", RunOpts{})
	if err != nil {
		t.Fatalf("expected fallback to reviewer-1, got error: %v", err)
	}
	findings, err := ParseFindings(result.Text)
	if err != nil {
		t.Fatalf("could not parse fallback output: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding from fallback, got %d", len(findings))
	}
	if len(findings[0].Sources) == 0 || findings[0].Sources[0] != "reviewer-1" {
		t.Errorf("fallback should set Sources to reviewer-1, got %v", findings[0].Sources)
	}
}
```

- [ ] **Step 2: Run to verify tests fail**

```bash
go test ./internal/review/ -run "TestBuildJudgePrompt|TestCouncilProvider" -v
```

Expected: FAIL — package does not compile (types don't exist yet).

- [ ] **Step 3: Create `internal/review/council.go`**

```go
package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// CouncilConfig holds the resolved provider/model settings for council mode.
// Constructed by runner.go after loading ReviewConfig; nil means council is off.
type CouncilConfig struct {
	ReviewerProvider string
	ReviewerModel    string
	JudgeProvider    string
	JudgeModel       string
}

// councilProvider implements ModelProvider by running two reviewers in parallel
// and having a judge arbitrate their findings.
type councilProvider struct {
	reviewer1 ModelProvider
	reviewer2 ModelProvider
	judge     ModelProvider
	r1Label   string // e.g. "anthropic/claude-opus-4-8"
	r2Label   string
	judgeLabel string
}

// newCouncilProvider constructs a councilProvider from the primary ModelConfig
// and the council fields in ReviewConfig. Applies fallback defaults.
func newCouncilProvider(primaryMC *ModelConfig, cfg *ReviewConfig, env []string) (*councilProvider, error) {
	// Reviewer 1: primary provider (already validated by config).
	r1MC := &ModelConfig{
		Provider:  primaryMC.Provider,
		Model:     primaryMC.Model,
		APIBase:   primaryMC.APIBase,
		APIKeyEnv: primaryMC.APIKeyEnv,
		ClaudeArgs: primaryMC.ClaudeArgs,
		ClaudePath: primaryMC.ClaudePath,
	}
	r1 := NewProviderForRole(r1MC, env)
	r1Label := r1MC.Provider + "/" + r1MC.Model

	// Reviewer 2: council_provider → codex fallback → primary provider.
	var r2MC *ModelConfig
	switch {
	case cfg.CouncilProvider != "":
		r2MC = &ModelConfig{Provider: cfg.CouncilProvider, Model: cfg.CouncilModel}
	case providerRegistered("codex"):
		r2MC = &ModelConfig{Provider: "codex", Model: GetSuggestedReviewModel("codex")}
	default:
		r2MC = &ModelConfig{
			Provider:  primaryMC.Provider,
			Model:     primaryMC.Model,
			APIBase:   primaryMC.APIBase,
			APIKeyEnv: primaryMC.APIKeyEnv,
			ClaudeArgs: primaryMC.ClaudeArgs,
			ClaudePath: primaryMC.ClaudePath,
		}
	}
	r2 := NewProviderForRole(r2MC, env)
	r2Label := r2MC.Provider + "/" + r2MC.Model

	// Judge: council_judge_provider → anthropic default.
	judgeProvider := cfg.CouncilJudgeProvider
	if judgeProvider == "" {
		judgeProvider = "anthropic"
	}
	judgeModel := cfg.CouncilJudgeModel
	if judgeModel == "" {
		judgeModel = "claude-opus-4-8"
	}
	if _, ok := providers[judgeProvider]; !ok {
		return nil, fmt.Errorf("council_judge_provider %q is not registered", judgeProvider)
	}
	judgeMC := &ModelConfig{Provider: judgeProvider, Model: judgeModel}
	judge := NewProviderForRole(judgeMC, env)
	judgeLabel := judgeProvider + "/" + judgeModel

	return &councilProvider{
		reviewer1:  r1,
		reviewer2:  r2,
		judge:      judge,
		r1Label:    r1Label,
		r2Label:    r2Label,
		judgeLabel: judgeLabel,
	}, nil
}

// providerRegistered reports whether a provider name is in the factory registry.
func providerRegistered(name string) bool {
	_, ok := providers[name]
	return ok
}

// Run fans the prompt out to both reviewers in parallel, then calls the judge.
func (p *councilProvider) Run(ctx context.Context, prompt string, opts RunOpts) (*providerResult, error) {
	type reviewerResult struct {
		result *providerResult
		err    error
	}

	ch1 := make(chan reviewerResult, 1)
	ch2 := make(chan reviewerResult, 1)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		r, err := p.reviewer1.Run(ctx, prompt, opts)
		ch1 <- reviewerResult{r, err}
	}()
	go func() {
		defer wg.Done()
		r, err := p.reviewer2.Run(ctx, prompt, opts)
		ch2 <- reviewerResult{r, err}
	}()

	wg.Wait()

	res1 := <-ch1
	res2 := <-ch2

	if res1.err != nil {
		fmt.Fprintf(os.Stderr, "Council: reviewer-1 (%s) failed: %v\n", p.r1Label, res1.err)
	}
	if res2.err != nil {
		fmt.Fprintf(os.Stderr, "Council: reviewer-2 (%s) failed: %v\n", p.r2Label, res2.err)
	}

	if res1.err != nil && res2.err != nil {
		return nil, fmt.Errorf("council: both reviewers failed: reviewer-1: %w; reviewer-2: %v", res1.err, res2.err)
	}

	// Parse findings from surviving reviewer(s).
	var r1Findings, r2Findings []Finding
	if res1.err == nil {
		r1Findings, _ = ParseFindings(res1.result.Text)
	}
	if res2.err == nil {
		r2Findings, _ = ParseFindings(res2.result.Text)
	}

	// Build usage list (reviewer calls).
	var modelUsages []CallUsage
	if res1.err == nil {
		u := res1.result.Usage
		u.Phase = "council-reviewer-1"
		u.DurationMS = res1.result.DurationMS
		modelUsages = append(modelUsages, u)
	}
	if res2.err == nil {
		u := res2.result.Usage
		u.Phase = "council-reviewer-2"
		u.DurationMS = res2.result.DurationMS
		modelUsages = append(modelUsages, u)
	}

	// Call judge.
	judgePrompt := BuildJudgePrompt(r1Findings, r2Findings)
	judgeRes, judgeErr := p.judge.Run(ctx, judgePrompt, opts)

	if judgeErr != nil {
		fmt.Fprintf(os.Stderr, "Council: judge (%s) failed — falling back to reviewer-1 output: %v\n", p.judgeLabel, judgeErr)
		return p.reviewer1Fallback(res1, r1Findings, modelUsages)
	}

	u := judgeRes.Usage
	u.Phase = "council-judge"
	u.DurationMS = judgeRes.DurationMS
	modelUsages = append(modelUsages, u)

	return &providerResult{
		Text:        judgeRes.Text,
		ModelUsages: modelUsages,
		DurationMS:  judgeRes.DurationMS,
	}, nil
}

// reviewer1Fallback returns reviewer-1's findings with Sources retroactively set.
func (p *councilProvider) reviewer1Fallback(res1 reviewerResult, r1Findings []Finding, modelUsages []CallUsage) (*providerResult, error) {
	if res1.err != nil {
		return nil, fmt.Errorf("council: judge failed and reviewer-1 also failed")
	}
	for i := range r1Findings {
		r1Findings[i].Sources = []string{"reviewer-1"}
	}
	text, err := marshalFindingsAsOutput(r1Findings)
	if err != nil {
		return nil, fmt.Errorf("council fallback: %w", err)
	}
	return &providerResult{Text: text, ModelUsages: modelUsages}, nil
}

// marshalFindingsAsOutput serializes findings into the standard ```json fence format.
func marshalFindingsAsOutput(findings []Finding) (string, error) {
	data, err := marshalFindingsJSON(findings)
	if err != nil {
		return "", err
	}
	return "```json\n" + string(data) + "\n```\n", nil
}

// marshalFindingsJSON marshals a findings slice to indented JSON bytes.
func marshalFindingsJSON(findings []Finding) ([]byte, error) {
	return json.MarshalIndent(findings, "", "  ")
}

// BuildJudgePrompt constructs the arbitration prompt for the judge provider.
// The judge sees both reviewer finding sets and is instructed to deduplicate,
// arbitrate, discard noise, and attribute each surviving finding.
func BuildJudgePrompt(r1Findings, r2Findings []Finding) string {
	r1JSON, _ := marshalFindingsJSON(r1Findings)
	r2JSON, _ := marshalFindingsJSON(r2Findings)

	var b strings.Builder
	b.WriteString(`You are a senior code review arbitrator. Two independent AI reviewers have analyzed the same pull request.
Your job: produce the best possible final review by arbitrating their findings.

Rules:
1. DEDUPLICATE: findings pointing to the same issue → keep one, merge sources field
2. ARBITRATE: solo findings → keep if the issue is real and significant; discard hallucinations, noise, and style opinions without substance
3. DISCARD: vague findings, findings on files not changed, findings with fabricated line numbers
4. ATTRIBUTE: set "sources" on every finding — ["reviewer-1"], ["reviewer-2"], or ["reviewer-1","reviewer-2"] for agreed findings

Output the findings as a JSON array inside a ` + "```" + `json code fence, using the same field names as the input.
Every finding MUST include a "sources" field.

--- REVIEWER 1 FINDINGS ---
`)
	b.WriteString("```json\n")
	b.Write(r1JSON)
	b.WriteString("\n```\n")
	b.WriteString("\n--- REVIEWER 2 FINDINGS ---\n")
	b.WriteString("```json\n")
	b.Write(r2JSON)
	b.WriteString("\n```\n")

	return b.String()
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/review/ -run "TestBuildJudgePrompt|TestCouncilProvider" -v
```

Expected: all 6 council tests pass.

- [ ] **Step 5: Run full suite**

```bash
go test ./internal/review/ -v 2>&1 | tail -20
```

Expected: all tests pass.

- [ ] **Step 6: Commit**

```bash
git add internal/review/council.go internal/review/council_test.go
git commit -m "feat(council): add councilProvider and BuildJudgePrompt"
```

---

## Task 4: Wire `CouncilEnabled` into `runner.go`

**Files:**
- Modify: `internal/review/runner.go:16-28` (RunOptions struct) and `~343-354` (provider construction)

**Depends on:** Task 3.

- [ ] **Step 1: Add `CouncilEnabled` to `RunOptions`**

In `internal/review/runner.go`, add one field to `RunOptions`:

```go
type RunOptions struct {
	Repo         string
	PRNumber     int
	ConfigPath   string
	Output       string
	Post         bool
	DryRun       bool
	ReplyOnly    bool
	ClaudePath   string
	Version      string
	PR           *PRData
	Platform     ReviewPlatform
	CouncilEnabled bool // when true, fan out to two reviewers + judge
}
```

- [ ] **Step 2: Construct `councilProvider` when enabled**

In `runner.go`, find the block that sets up `reviewProvider` (around line 351):

```go
reviewProvider := NewProviderForRole(reviewMC, rctx.Env)
```

Replace with:

```go
var reviewProvider ModelProvider
if opts.CouncilEnabled {
    cp, err := newCouncilProvider(reviewMC, cfg, rctx.Env)
    if err != nil {
        return fmt.Errorf("council setup: %w", err)
    }
    reviewProvider = cp
    Stderrf(ansiBold, "Council mode: %s vs %s, judge %s\n", cp.r1Label, cp.r2Label, cp.judgeLabel)
} else {
    reviewProvider = NewProviderForRole(reviewMC, rctx.Env)
}
```

- [ ] **Step 3: Add dry-run council message**

In `runner.go`, find the dry-run return block (around line 392):

```go
if opts.DryRun {
    if prompt != "" {
        fmt.Print(prompt)
    }
    return nil
}
```

Replace with:

```go
if opts.DryRun {
    if prompt != "" {
        fmt.Print(prompt)
    }
    if opts.CouncilEnabled {
        // councilProvider is already constructed above; labels are available.
        if cp, ok := reviewProvider.(*councilProvider); ok {
            fmt.Fprintf(os.Stderr, "[council mode: would fan out to %s and %s, judged by %s]\n",
                cp.r1Label, cp.r2Label, cp.judgeLabel)
        }
    }
    return nil
}
```

- [ ] **Step 4: Build and run tests**

```bash
go build ./cmd/review && go test ./internal/review/ -v 2>&1 | tail -20
```

Expected: build succeeds, all tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/review/runner.go
git commit -m "feat(council): wire CouncilEnabled into runner.go"
```

---

## Task 5: Add `--council` flag to the CLI

**Files:**
- Modify: `cmd/review/cli/review.go`

**Depends on:** Task 4.

- [ ] **Step 1: Add flag and set `CouncilEnabled` in both code paths**

In `cmd/review/cli/review.go`, in the `RunE` function, add after the existing flag reads:

```go
council, _ := cmd.Flags().GetBool("council")
```

Then in the GitHub mode `review.Run(...)` call, add:
```go
CouncilEnabled: council,
```

And in the local mode `review.Run(...)` call, add:
```go
CouncilEnabled: council,
```

In `init()`, add:
```go
reviewCmd.Flags().Bool("council", false, "Fan out to two reviewers in parallel and have a judge arbitrate the findings")
```

- [ ] **Step 2: Build and smoke-test**

```bash
go build ./cmd/review -o /tmp/codecanary-test
/tmp/codecanary-test review --help | grep council
```

Expected: `--council` appears in the help output.

- [ ] **Step 3: Run all tests**

```bash
go test ./... 2>&1 | tail -10
```

Expected: all tests pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/review/cli/review.go
git commit -m "feat(council): add --council CLI flag"
```

---

## Task 6: Render `Sources` attribution in formatter

**Files:**
- Modify: `internal/review/formatter.go` (terminal and markdown)
- Modify: `internal/review/formatter_test.go`

**Depends on:** Task 1 (Sources field).

- [ ] **Step 1: Write failing formatter tests**

Add to `internal/review/formatter_test.go`:

```go
func TestFormatTerminalSourcesAgreed(t *testing.T) {
	result := &ReviewResult{
		Findings: []Finding{
			{ID: "f1", File: "a.go", Line: 1, Severity: "warning", Title: "T", Description: "D",
				Sources: []string{"reviewer-1", "reviewer-2"}},
		},
	}
	out := FormatTerminal(result)
	if !strings.Contains(out, "[agreed]") {
		t.Errorf("expected [agreed] tag for two-source finding, got:\n%s", out)
	}
}

func TestFormatTerminalSourcesSolo(t *testing.T) {
	result := &ReviewResult{
		Findings: []Finding{
			{ID: "f1", File: "a.go", Line: 1, Severity: "warning", Title: "T", Description: "D",
				Sources: []string{"reviewer-2"}},
		},
	}
	out := FormatTerminal(result)
	if !strings.Contains(out, "[reviewer-2]") {
		t.Errorf("expected [reviewer-2] tag for solo finding, got:\n%s", out)
	}
}

func TestFormatTerminalSourcesAbsent(t *testing.T) {
	// Single-reviewer findings (no Sources) should render without any attribution tag.
	result := &ReviewResult{
		Findings: []Finding{
			{ID: "f1", File: "a.go", Line: 1, Severity: "warning", Title: "T", Description: "D"},
		},
	}
	out := FormatTerminal(result)
	if strings.Contains(out, "[agreed]") || strings.Contains(out, "[reviewer-") {
		t.Errorf("single-reviewer finding should have no attribution tag, got:\n%s", out)
	}
}

func TestFormatMarkdownSources(t *testing.T) {
	result := &ReviewResult{
		PRNumber: 1,
		Findings: []Finding{
			{ID: "f1", File: "a.go", Line: 1, Severity: "bug", Title: "T", Description: "D",
				Sources: []string{"reviewer-1", "reviewer-2"}},
		},
	}
	out := FormatMarkdown(result)
	if !strings.Contains(out, "[agreed]") {
		t.Errorf("markdown should contain [agreed] tag, got:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
go test ./internal/review/ -run "TestFormatTerminalSources|TestFormatMarkdownSources" -v
```

Expected: FAIL with a **compile error** — `sourceTag undefined`. This is the correct TDD signal; the package won't compile until the helper is added in Step 3.

- [ ] **Step 3: Add `sourceTag` helper to `formatter.go`**

Add after `statusTag`:

```go
// sourceTag returns an attribution label for council-mode findings.
// Returns empty string when Sources is nil (single-reviewer mode).
func sourceTag(sources []string, colors bool) string {
	if len(sources) == 0 {
		return ""
	}
	if len(sources) >= 2 {
		return applyStyle(colors, ansiGreen, "[agreed]")
	}
	return applyStyle(colors, ansiCyan, "["+sources[0]+"]")
}
```

- [ ] **Step 4: Render tag in `writeTerminalFinding`**

In `writeTerminalFinding`, find the header line construction:

```go
header := fmt.Sprintf("  %s %s  %s", dot, sevLabel, findingID)
if tag := statusTag(f.Status, colors); tag != "" {
    header += "  " + tag
}
```

Add after the `statusTag` block:

```go
if tag := sourceTag(f.Sources, colors); tag != "" {
    header += "  " + tag
}
```

- [ ] **Step 5: Render tag in `FormatMarkdown`**

In `FormatMarkdown`, find the finding header line:

```go
fmt.Fprintf(&b, "### %s `%s` in `%s:%d`\n", icon, f.ID, f.File, f.Line)
```

Replace with:

```go
srcTag := ""
if tag := sourceTag(f.Sources, false); tag != "" {
    srcTag = " " + tag
}
fmt.Fprintf(&b, "### %s `%s` in `%s:%d`%s\n", icon, f.ID, f.File, f.Line, srcTag)
```

- [ ] **Step 6: Run formatter tests**

```bash
go test ./internal/review/ -run "TestFormat" -v
```

Expected: all formatter tests pass.

- [ ] **Step 7: Run full suite**

```bash
go test ./... 2>&1 | tail -10
```

Expected: all tests pass.

- [ ] **Step 8: Commit**

```bash
git add internal/review/formatter.go internal/review/formatter_test.go
git commit -m "feat(council): render sources attribution tag in terminal and markdown output"
```

---

## Task 7: Update `docs/review-flow.md`

**Files:**
- Modify: `docs/review-flow.md`

**Depends on:** all prior tasks complete.

- [ ] **Step 1: Add a Council Mode section**

Open `docs/review-flow.md` and add a new section after the existing "Pipeline Steps" section. The section should explain:

1. How `--council` activates `councilProvider` in step 7 of the pipeline
2. The three-provider flow: reviewer-1 (primary), reviewer-2 (codex → fallback), judge
3. Config fields (`council_provider`, `council_model`, `council_judge_provider`, `council_judge_model`)
4. Fallback behavior: one reviewer fails → judge runs with single input; judge fails → reviewer-1 output with `Sources` retroactively set
5. The `sources` field on `Finding` and what `[agreed]` / `[reviewer-1]` / `[reviewer-2]` mean in output

Content (add verbatim):

```markdown
## Council Mode (`--council`)

Council mode fans the review prompt out to **two independent reviewers** in parallel, then runs a **judge** that arbitrates the findings — deduplicating overlapping issues, filtering noise, and attributing each surviving finding to its source(s).

### Activation

Pass `--council` to any `codecanary review` invocation. Works in both local and `--post` (GitHub) modes.

### Three-provider flow

```
prompt
  ├─► reviewer-1 (primary provider/model from config)
  └─► reviewer-2 (council_provider/council_model, or codex fallback, or primary)
        ↓ both complete (or one degrades gracefully)
BuildJudgePrompt(r1 findings, r2 findings)
        ↓
judge (council_judge_provider/council_judge_model, default: anthropic/claude-opus-4-8)
        ↓
final findings with "sources" attribution
```

### Config

All fields are optional. Add to `.codecanary/config.yml`:

```yaml
council_provider: codex           # 2nd reviewer provider (default: codex if registered, else primary)
council_model: gpt-5.4-codex      # 2nd reviewer model
council_judge_provider: anthropic # judge provider (default: anthropic)
council_judge_model: claude-opus-4-8 # judge model
```

### Attribution

The judge sets a `sources` field on every finding:
- `["reviewer-1", "reviewer-2"]` → rendered as `[agreed]` — both reviewers raised this issue
- `["reviewer-1"]` or `["reviewer-2"]` → rendered as `[reviewer-1]` / `[reviewer-2]` — solo finding kept by the judge

### Degradation

| Failure | Behaviour |
|---|---|
| Reviewer-1 fails | Judge runs with reviewer-2's findings only |
| Reviewer-2 fails | Judge runs with reviewer-1's findings only |
| Both fail | Error returned, no output |
| Judge fails | Reviewer-1's findings returned with `sources: ["reviewer-1"]` on each |

### Usage tracking

All three calls appear in the usage table with phase labels `council-reviewer-1`, `council-reviewer-2`, and `council-judge`, and roll up to the normal totals.
```

- [ ] **Step 2: Verify the doc renders correctly**

```bash
cat docs/review-flow.md | grep -A 5 "Council Mode"
```

Expected: section header and description appear.

- [ ] **Step 3: Build one final time**

```bash
go build ./... && go test ./... 2>&1 | tail -10
```

Expected: clean build, all tests pass.

- [ ] **Step 4: Final commit**

```bash
git add docs/review-flow.md
git commit -m "docs: document council mode in review-flow.md"
```

---

## Final Verification

After all tasks are committed:

```bash
# Build the binary
go build ./cmd/review -o /tmp/codecanary-council

# Confirm --council flag is visible
/tmp/codecanary-council review --help | grep -A 1 council

# Confirm --dry-run prints council note
/tmp/codecanary-council review --dry-run --council 2>&1 | grep -i council

# Full test suite
go test ./... -count=1
```

Expected: binary builds, `--council` appears in help, dry-run prints council plan, all tests green.
