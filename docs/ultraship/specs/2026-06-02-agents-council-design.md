# Agents Council — Design Spec

**Date:** 2026-06-02
**Status:** Approved

## Summary

Agents Council is an opt-in mode (`--council` CLI flag) that fans out the same review prompt to two independent AI reviewers in parallel, then runs a third judge agent that arbitrates their findings — deduplicating, filtering noise, and attributing each surviving finding to its source(s). The result is a single high-confidence findings list, richer than any single reviewer alone.

---

## Architecture

Council mode is implemented as a `councilProvider` struct that implements the existing `ModelProvider` interface. `runner.go` is nearly untouched — it constructs a `councilProvider` when `opts.Council != nil` and otherwise behaves identically.

### New files

| File | Purpose |
|---|---|
| `internal/review/council.go` | `councilProvider`, `CouncilConfig`, `BuildJudgePrompt` |
| `internal/review/council_test.go` | Unit tests |

### Modified files

| File | Change |
|---|---|
| `internal/review/findings.go` | Add `Sources []string` field to `Finding` |
| `internal/review/config.go` | Add 4 council config fields to `ReviewConfig` |
| `internal/review/runner.go` | Construct `councilProvider` when `opts.Council != nil` |
| `cmd/review/cli/review.go` | Add `--council` boolean flag |
| `docs/review-flow.md` | Document the council path |

### Flow inside `councilProvider.Run()`

```
prompt
  ├─► reviewer-1 goroutine → raw text + usage
  └─► reviewer-2 goroutine → raw text + usage
        ↓ (both complete or one fails gracefully)
ParseFindings(r1), ParseFindings(r2)
        ↓
BuildJudgePrompt(r1 findings, r2 findings)
        ↓
judge provider → final findings JSON with sources attribution
        ↓
providerResult{Text: judgeOutput, ModelUsages: [r1, r2, judge]}
```

`runner.go`'s step 7 (`reviewProvider.Run()`) is unchanged — it receives a `providerResult` and passes the text to `processFindings()` as today. The council is invisible to the rest of the pipeline.

---

## Config & CLI

### CLI

```
codecanary review [PR#] --council
```

`--council` is a boolean flag. Works in both local and GitHub modes. Does not conflict with any existing flag.

### Config fields (all optional)

```yaml
# Second reviewer (defaults: codex if registered, else primary provider)
council_provider: codex
council_model: gpt-5.4-codex

# Judge (defaults: anthropic + claude-opus-4-8)
council_judge_provider: anthropic
council_judge_model: claude-opus-4-8
```

### Second-reviewer fallback logic

1. `council_provider` is set → use it (error if provider not registered)
2. `codex` is registered → use codex with its suggested review model
3. Otherwise → use primary provider + `review_model` (two parallel calls to the same model — still useful for noise reduction)

### Internal wiring

`CouncilConfig` is a new struct stored on `RunOptions`:

```go
type CouncilConfig struct {
    ReviewerProvider string
    ReviewerModel    string
    JudgeProvider    string
    JudgeModel       string
}
```

`opts.Council == nil` means council is off. `runner.go` checks this once when constructing the review provider.

---

## Judge Prompt & Output Format

### `Finding` struct addition

```go
Sources []string `json:"sources,omitempty"` // ["reviewer-1"], ["reviewer-2"], ["reviewer-1","reviewer-2"]
```

Backward-compatible — existing findings without `sources` parse cleanly; `omitempty` means single-reviewer output is unchanged.

### Judge prompt (`BuildJudgePrompt`)

```
You are a senior code review arbitrator. Two independent AI reviewers have analyzed the same PR.
Your job: produce the best possible final review by arbitrating their findings.

Rules:
1. DEDUPLICATE: findings pointing to the same issue → keep one, merge sources
2. ARBITRATE: solo findings → keep if the issue is real and significant; discard hallucinations, noise, style opinions without substance
3. DISCARD: vague findings, findings on files not in the diff, findings with fabricated line numbers
4. ATTRIBUTE: set "sources" to which reviewer(s) raised it — ["reviewer-1"], ["reviewer-2"], or ["reviewer-1","reviewer-2"]

Output the same JSON format as the reviewers, including "sources" on every finding.

--- REVIEWER 1 FINDINGS ---
<r1 findings JSON>

--- REVIEWER 2 FINDINGS ---
<r2 findings JSON>
```

The original diff/prompt is **not** re-sent to the judge — it operates only on structured finding sets, keeping the judge prompt small and focused.

### Usage tracking

`providerResult.ModelUsages` carries all three entries with distinct phase labels:
- `"council-reviewer-1"`
- `"council-reviewer-2"`
- `"council-judge"`

### Terminal/markdown output

Each finding header gets a source tag: `[agreed]`, `[reviewer-1]`, or `[reviewer-2]`. The formatter adds a small render path for the `Sources` field.

---

## Error Handling & Edge Cases

### One reviewer fails

- Log warning to stderr; proceed with the surviving reviewer's findings.
- The judge still runs with a single input — it still deduplicates and filters that set.

### Both reviewers fail

- Return the combined error. No output.

### Judge fails

- Fall back to reviewer-1's findings as final output.
- Log: `"Council judge failed — using reviewer-1 output"`.
- Rationale: never leave the user with nothing.

### Incremental reviews (triage)

- Triage runs before council using the primary `triageProvider`, unchanged.
- Council only affects the main review LLM call (step 7 of `Run()`).
- Both reviewers receive the same prompt (full or incremental diff).

### Budget

- `CheckBudget` runs once before the council fan-out, same as today.
- Each of the three calls tracks cost separately via `ModelUsages`.
- No per-call budget split — the full budget is available to each call.

### `--dry-run --council`

- Prints the review prompt once (as today) plus:
  `"[council mode: would fan out to <provider1> and <provider2>, judged by <judge>]"`
- No LLM calls are made.

---

## What Is Not Changing

- `runner.go` core pipeline (steps 1–6, 8–12) — unchanged
- `triage.go` / triage flow — unchanged
- `ReviewPlatform` interface — unchanged
- All existing provider implementations — unchanged
- Single-reviewer behavior when `--council` is not passed — unchanged
- Telemetry schema — the three `ModelUsages` entries roll up to the existing `totalIn/totalOut/totalCost` aggregation naturally
