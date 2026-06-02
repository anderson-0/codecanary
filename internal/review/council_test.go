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
