---
name: feedback-verify-dont-recall
description: In this repo a finding is only a finding once measured — run the tree, cite file:line, date the claim; and never redact docs as the reviewer.
metadata:
  type: feedback
---

Measure, then write. An unverified finding is a hypothesis and must be labelled
one.

**Why:** the project's house style is measured claims with dates ("MEASURED
2026-08-24: ..."), and the docs themselves are written that way. Reviews are held
to the same bar, which is also what makes them useful: the two most valuable
findings of the 2026-09-09 gate review both came from running things (a local
mpv/loopback experiment, and querying proxy.golang.org for real module versions),
not from reading.

**How to apply:**
- Always run `go vet ./...`, `go test ./...` and `smoke.ps1` before writing, and
  report the real numbers. Baseline as of 2026-09-09: vet clean, 131 top-level
  tests (core 48, net 29, resolver 21, player 14, store 13, cmd 6), smoke 35/35.
  A number that differs from a number a doc asserts is itself a finding.
- Never run `-tags integration` and never add a smoke case that performs real
  egress — those open a live circuit.
- Reports are append-only under `ch-writings/report-review/`; never edit a past
  one. Never redact `SKILL.md` or `INSTRUCTIONS.md` as the reviewer, even when the
  check-up protocol permits it — recommend it in the Fixes board instead.
- Doc drift is a first-class finding class here: the same stale sentence tends to
  live in three or four files at once (TASKS.md, INSTRUCTIONS.md, the skill, and
  a Go doc comment). Grep the phrase across all of them before reporting one.

Related: [[project-mpv-proxy-env-trap]], [[user-chainsmoke-dev]].
