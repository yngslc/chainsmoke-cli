---
name: project-phase10-open-decisions
description: Phase 10 (TUI) carries three unresolved decisions — T10.3 library, T10.5 screen inventory, go-winio — and the T10.3 research in PHASE10.md was measured against the v1 Charm line.
metadata:
  type: project
---

Phase 10 is the current phase as of 2026-09-09. Three decisions are held open on
purpose and are the dev's to make, not the reviewer's:

1. **T10.3 — TUI library.** Bubble Tea + lipgloss + bubbles vs tview/tcell.
   `ch-writings/PHASE10.md` Part 2 has the comparison, measured 2026-08-24.
2. **T10.5 — screen inventory.** Proposal in PHASE10.md Part 3; the dev asked for
   "a full scale interface — search, likes, playlists, then privacy-config".
3. **go-winio** for cancellable named-pipe reads on Windows (needed by the
   event-reading IPC client, T10.7). Stdlib `os.OpenFile` can talk to the pipe but
   a blocked read cannot be interrupted by `Close()`.

**Why it matters:** PHASE10.md's T10.3 table names the **v1** Charm modules
(bubbletea v1.3.10, lipgloss v1.1.0, bubbles v1.0.0). Checked against
proxy.golang.org on 2026-09-09, separate **v2** module paths were already
published before that table was written: `bubbletea/v2` v2.0.9 (2026-08-19),
`lipgloss/v2` v2.0.6 (2026-08-11), `bubbles/v2` v2.2.1 (2026-08-24), while
bubbletea v1's last release was 2025-09-17. tview v0.42.0 (2025-08-27) is still
its latest tag.

**How to apply:** if the dev is about to commit to a library, say that the linked
module count and the widget API both differ between the v1 and v2 lines, and that
the spike has to be run against whichever line is chosen — re-check the versions
at that moment rather than quoting these. Do not make the call for them.

Related: [[feedback-verify-dont-recall]].
