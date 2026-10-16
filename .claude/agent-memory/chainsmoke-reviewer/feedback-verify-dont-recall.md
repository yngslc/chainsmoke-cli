---
name: feedback-verify-dont-recall
description: In this repo a finding is only a finding once measured — run the tree, cite file:line, date the claim; never redact docs as the reviewer; how to measure package-main behavior without touching the repo.
metadata:
  type: feedback
---

Measure, then write. An unverified finding is a hypothesis and must be labelled
one.

**Why:** the project's house style is measured claims with dates ("MEASURED
2026-08-24: ..."), and the docs themselves are written that way. Reviews are held
to the same bar, which is also what makes them useful: the most valuable
findings of the 09-09, 10-01 and 10-05 reviews all came from running things
(loopback mpv rigs, proxy.golang.org queries, a scratch-copy test rig), not
from reading.

**How to apply:**
- Always run `go vet ./...`, `go test ./...` and `smoke.ps1` before writing, and
  report the real numbers. Baseline as of 2026-10-05 (HEAD 175a252): vet clean;
  Windows 167 pass + 3 unix-only skips (core 51, net 36, resolver 23, player 22,
  store 13, cmd 22); WSL `-race` 170 pass, 0 races; smoke 40/40. A number that
  differs from a number a doc asserts is itself a finding.
- From the Bash tool (Git Bash), call smoke with forward slashes:
  `powershell -File "D:/sccli/.claude/skills/run-chainsmoke/smoke.ps1"`.
  Backslashes get eaten and PowerShell reports a missing file (cost one run).
- To measure `cmd/chainsmoke` internals (queue.go/session.go are package main)
  without editing the repo: copy `go.mod go.sum cmd pkg` into the scratchpad,
  add a `rig_test.go` there that reuses the package's own fakes, run `go test`.
  io.Pipe models os.Stdin contention faithfully (both serialize readers).
- Never run `-tags integration` and never add a smoke case that performs real
  egress — those open a live circuit.
- Reports are append-only under `ch-writings/report-review/`; never edit a past
  one. Never redact `SKILL.md` or `INSTRUCTIONS.md` as the reviewer, even when the
  check-up protocol permits it — recommend it in the Fixes board instead.
- Doc drift is a first-class finding class here: the same stale sentence tends to
  live in three or four files at once (TASKS.md, INSTRUCTIONS.md, the skill, and
  a Go doc comment — `pkg/net/doc.go` is the T2.7 caveat list and drifts too).
  Grep the phrase across all of them before reporting one.

Related: [[project-mpv-proxy-env-trap]], [[user-chainsmoke-dev]].
