---
name: feedback-verdict-required
description: The dev wants a go/no-go verdict at the end of every debate report, not just a list of objections.
metadata:
  type: feedback
---

Every debate report must end with an explicit verdict on whether work can
proceed. The dev's phrasing on 2026-09-09: *"if there's nothing to work on —
drop the flag, call the development of TUI."*

**Why:** the debater runs last in the queue, after the Reviewer. A report that
only raises objections leaves the dev with two documents and no decision. The
dev explicitly asked for the flag to be dropped when objections would not change
what happens next — so "I found nothing that blocks you" is a valid and wanted
outcome, not a failure of the role.

**How to apply:** close the Summary with (a) the two or three objections that
would change next week's work, and (b) a plain go / no-go on the current phase
gate. If none of the objections change anything, say so and give the go-ahead in
those words. Rank objections by cost-to-fix-later so the verdict is legible.

Related: [[project-house-style-measured-claims]]
