---
name: project-phase10-open-decisions
description: Phase 10 (TUI) open decisions — CLI-only release vs TUI (asked 2026-10-05), T10.3 library, T10.5 screens, go-winio; the T10.3 data was measured against Charm v1; the play session already is a line-mode session model.
metadata:
  type: project
---

Phase 10 is still the current phase as of 2026-10-05, and no TUI code exists
(`cmd/chainsmoke/tui.go` is an empty stub). Decisions held open, the dev's to
make, not the reviewer's:

0. **CLI-only release now vs keep building the TUI (asked 2026-10-05).** Since
   175a252 the `play` session prompt (session.go + queue.go, package main) does
   list playback, shuffle/repeat and track-change announcements — a line-mode
   version of the session model INSTRUCTIONS.md:124 assigns to the TUI. The
   reviewer's 2026-10-05 read: release the CLI first, IN THIS REPO (a separate
   project would fork the egress chokepoint); keep the TUI as Phase 10 here;
   first extract listQueue into pkg/ so both front-ends share it, as the
   "second front-end, never a parallel implementation" rule requires.
1. **T10.3 — TUI library.** Bubble Tea + lipgloss + bubbles vs tview/tcell.
   `ch-writings/PHASE10.md` Part 2 has the comparison.
2. **T10.5 — screen inventory.** Proposal in PHASE10.md Part 3.
3. **go-winio** for cancellable named-pipe reads on Windows (T10.7).

**Why it matters:** PHASE10.md's T10.3 table originally named the **v1** Charm
modules; separate **v2** module paths (`charm.land/bubbletea/v2` etc.) had
shipped by 2026-08. tview v0.42.0 was its latest tag on 2026-09-09.

**How to apply:** at the next review, first check TASKS.md for a dated
`dev's conclusion` on decision 0, and scope to it. If the dev is about to pick a
library, say that module count and widget API differ between Charm v1 and v2,
and re-check versions at that moment rather than quoting these.

Related: [[feedback-verify-dont-recall]], [[user-chainsmoke-dev]].
