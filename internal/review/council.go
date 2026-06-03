package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// councilProvider implements ModelProvider by running two reviewers in parallel
// and having a judge arbitrate their findings.
type councilProvider struct {
	reviewer1  ModelProvider
	reviewer2  ModelProvider
	judge      ModelProvider
	r1Label    string
	r2Label    string
	judgeLabel string
}

// reviewerResult bundles a reviewer's output with any error.
type reviewerResult struct {
	result *providerResult
	err    error
}

// newCouncilProvider constructs a councilProvider from the primary ModelConfig
// and the council fields in ReviewConfig. Applies fallback defaults.
func newCouncilProvider(primaryMC *ModelConfig, cfg *ReviewConfig, env []string) (*councilProvider, error) {
	r1MC := &ModelConfig{
		Provider:     primaryMC.Provider,
		Model:        primaryMC.Model,
		AdvisorModel: primaryMC.AdvisorModel,
		APIBase:      primaryMC.APIBase,
		APIKeyEnv:    primaryMC.APIKeyEnv,
		ClaudeArgs:   primaryMC.ClaudeArgs,
		ClaudePath:   primaryMC.ClaudePath,
	}
	r1 := NewProviderForRole(r1MC, env)
	r1Label := r1MC.Provider + "/" + r1MC.Model

	var r2MC *ModelConfig
	switch {
	case cfg.CouncilProvider != "":
		m := cfg.CouncilModel
		if m == "" {
			m = GetSuggestedReviewModel(cfg.CouncilProvider)
		}
		r2MC = &ModelConfig{Provider: cfg.CouncilProvider, Model: m}
	case providerRegistered("codex"):
		r2MC = &ModelConfig{Provider: "codex", Model: GetSuggestedReviewModel("codex")}
	default:
		r2MC = &ModelConfig{
			Provider:   primaryMC.Provider,
			Model:      primaryMC.Model,
			APIBase:    primaryMC.APIBase,
			APIKeyEnv:  primaryMC.APIKeyEnv,
			ClaudeArgs: primaryMC.ClaudeArgs,
			ClaudePath: primaryMC.ClaudePath,
		}
	}
	r2 := NewProviderForRole(r2MC, env)
	r2Label := r2MC.Provider + "/" + r2MC.Model

	judgeProvider := cfg.CouncilJudgeProvider
	if judgeProvider == "" {
		judgeProvider = primaryMC.Provider
	}
	judgeModel := cfg.CouncilJudgeModel
	if judgeModel == "" {
		switch judgeProvider {
		case "claude":
			judgeModel = "opus"
		case "anthropic":
			judgeModel = "claude-opus-4-8"
		default:
			judgeModel = GetSuggestedReviewModel(judgeProvider)
		}
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

	var r1Findings, r2Findings []Finding
	if res1.err == nil {
		r1Findings, _ = ParseFindings(res1.result.Text)
	}
	if res2.err == nil {
		r2Findings, _ = ParseFindings(res2.result.Text)
	}

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

	judgePrompt := buildJudgePrompt(r1Findings, r2Findings)
	judgeRes, judgeErr := p.judge.Run(ctx, judgePrompt, opts)

	if judgeErr != nil {
		fmt.Fprintf(os.Stderr, "Council: judge (%s) failed — falling back to best available reviewer: %v\n", p.judgeLabel, judgeErr)
		return p.reviewerFallback(res1, res2, r1Findings, r2Findings, modelUsages)
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

// reviewerFallback returns findings from the first available reviewer when the
// judge fails, preferring reviewer-1. Sources are set retroactively.
func (p *councilProvider) reviewerFallback(res1, res2 reviewerResult, r1Findings, r2Findings []Finding, modelUsages []CallUsage) (*providerResult, error) {
	var findings []Finding
	var label string
	switch {
	case res1.err == nil:
		findings = r1Findings
		label = "reviewer-1"
	case res2.err == nil:
		findings = r2Findings
		label = "reviewer-2"
	default:
		return nil, fmt.Errorf("council: judge failed and both reviewers also failed")
	}
	for i := range findings {
		findings[i].Sources = []string{label}
	}
	text, err := marshalFindingsAsOutput(findings)
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

// buildJudgePrompt constructs the arbitration prompt for the judge provider.
func buildJudgePrompt(r1Findings, r2Findings []Finding) string {
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
