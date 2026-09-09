---
name: project-house-style-measured-claims
description: chainsmoke's docs assert dated, reproducible measurements; contesting one requires re-running it, and most re-run in one command.
metadata:
  type: project
---

Claims in this project's docs (INSTRUCTIONS.md, PHASE10.md, TASKS.md, the
run-chainsmoke SKILL.md, and code comments) are written as *measurements with
dates* — "measured 2026-08-24", "verified against mpv 0.41". They are almost
always reproducible in one command.

**Why:** it is a deliberate house style, and it is what makes the tree
auditable. It also means a stale measurement is the project's most common defect
class: the number was right on the day and nobody is assigned to re-check it.
Confirmed across several rounds — re-running dated claims reproduced them
exactly (the `-trimpath` 15-paths claim, PHASE10.md's 25-module count), while
version-dependent ones had drifted (tcell dropping `mattn/go-runewidth`,
Charm v2 moving to `charm.land/*`).

**How to apply:** never assert a measured fact is false without a
counter-measurement — run it, and report the exact command. When a claim cannot
be re-run here, file it as MEASUREMENT tier and say explicitly that it was not
run. Re-running claims that turn out to be *correct* is not wasted work: the
"Unchallenged" section is what makes the contested findings credible.

Related: [[feedback-verdict-required]], [[project-measurement-environment]]
