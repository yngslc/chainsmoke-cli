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

**Headline numbers are reliable; safety *wording* is where the drift is.**
2026-10-19, pre-public-drop: every number the main agent quoted reproduced
exactly (gofmt/vet clean; Windows 173 pass/3 skip/0 fail; WSL `-race` 176/0/0;
smoke 40/40), and mutation-reverting the R5 switch order and the R10 liveness
check both turned the suite red as claimed. What did *not* survive was the
prose: `audit` printing `TOR ✓ (verified by a live CONNECT)` for any peer that
completes one CONNECT (no Tor-identity check exists), the README's "track URLs
never go on a command line" (the permalink and the bridge port are in yt-dlp's
argv every resolve), `ConnectBridge.Close()`'s "narrow" limitation (it blocks
indefinitely after a failed *preflight*, no mpv involved), and `--private`
writing `<runtime>/last-newnym`. **One claimed mutation check was absent**:
deleting `landed()`'s `playlist-count` guard left `go test ./cmd/...` green.
So: trust the counts, audit the adjectives, and spot-check "mutation-checked"
by actually mutating. Re-verify all of this before citing it — it should be
fixed.

Related: [[feedback-verdict-required]], [[project-measurement-environment]]
