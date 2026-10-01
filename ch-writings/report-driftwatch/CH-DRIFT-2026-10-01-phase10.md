# Driftwatch 2026-10-01 — Phase 10 (all items are hypotheses)

**Mechanical**: go vet clean. go test ./... Windows: 149 pass, 2 skip (matches 149+2). Not run here: smoke.ps1, WSL -race (main agent: 40/40, 151).
Counts agree in SKILL.md:155. Stale: "35 cases" in TASKS.md:508 and PHASE10.md:165 (the latter is struck through and dated, so likely fine). CH-2026-09-09 report says 35 (historical).

**Candidates**
- PHASE10.md:5-6 — "explicit verbs stay one-shot and quiet" with no 2026-09-28 banner note; sibling INSTRUCTIONS.md:89-91 was corrected. low-medium
- PHASE10.md:64-66 — "any verb -> today's one-shot path", banner rule only on -h; verbs-on-TTY banner not mentioned. low
- TASKS.md:515-517, 531-535, 658-660 — still the old banner routing; no dated dev's conclusion for 2026-09-28 call (review L-a still open). medium
- TASKS.md:508 — "35 cases" present tense, undated; smoke is 40. medium
- build.ps1:33 comment — "drive letter and directory layout (and," appears updated to mention the username; the review's C-7 may be fixed, but SKILL.md:77-83 already says it checks. Looks closed. low
- main.go:365 / SKILL.md:261 — "sends no SoundCloud traffic of its own" are corrected consistently; nothing found.

**Still-open review/debate doc items** (not re-adjudicated): L-a (banner docs in TASKS/PHASE10, above). R-9 (bare-invocation smoke row) is covered by SKILL.md's `bare-pipe` mention, so possibly closed. L-b (`play` Ensure order) is documented as fixed in SKILL.md. R-12 reviewer Edit grant, RR-4 stale dist/, S-2, C-9 not checked deeply.

**Escalate**: no_proxy/http:// bypass closures, audit DNS probe, Ensure strays and L-c: egress/privacy territory, Reviewer's bar. I only checked that the doc wording is consistent across files.

**Recommendation**: Mostly quiet. A doc-only cleanup of the TASKS.md/PHASE10.md banner sentences and the 35-case count is worthwhile. No full Reviewer pass needed for drift alone.
