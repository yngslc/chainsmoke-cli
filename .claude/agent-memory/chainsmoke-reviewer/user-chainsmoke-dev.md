---
name: user-chainsmoke-dev
description: The dev is chainsmoke's sole author, works phase-by-phase off TASKS.md, and settles design questions with a "dev's conclusion" verdict that overrides the task text above it.
metadata:
  type: user
---

Single developer (`yngslc`), building chainsmoke alone and driving it phase by
phase from `ch-writings/TASKS.md`. Deep Go; comfortable with SOCKS/Tor internals,
mpv IPC, and Windows/WSL dual-target work.

How they work, and what it means for a review:

- **`**dev's conclusion: ...**` is the verdict.** Where it contradicts the task
  body above it, the verdict wins and the body is history. Do not report a task
  as incomplete because its prose still describes the rejected option.
- **Decisions are recorded with reasons and dates, and they expect the same
  back.** Long "why" comments in the code are intentional documentation, not
  noise — do not suggest trimming them.
- **The ASCII banner stays.** They want the CLI's visual presentation enhanced,
  not stripped; the banner's *placement* is T10.1/T10.2's to fix, its existence
  is not up for debate.
- **Dependency minimalism is a stated value** — one direct module
  (`urfave/cli/v2`) is "a property worth defending". Any recommendation that adds
  modules needs the trade named explicitly.
- Dev environment: Windows 10 host, Go via a portable install, tor inside WSL
  reachable on `127.0.0.1:9050` with **no ControlPort** (so `rotate` can only
  re-isolate SOCKS credentials here), mpv 0.41 and yt-dlp on the User PATH, and
  `git` reachable only through WSL from a native shell.

Related: [[feedback-verify-dont-recall]].
