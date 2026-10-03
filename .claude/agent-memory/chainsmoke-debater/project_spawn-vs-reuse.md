---
name: project-spawn-vs-reuse
description: In chainsmoke, child-process safety fixes are spawn-time properties but the mpv daemon is reused across runs and builds — always test the reuse path and the process-group path.
metadata:
  type: project
---

Recurring blind spot (found 2026-10-03 against `9c6a7fd`): fixes to how mpv is
spawned (env filtering, flags like `--no-ytdl`) only protect daemons the
current build spawns. `Ensure()` reuses ANY live daemon on the endpoint with no
check, and under T0.1(a) the daemon outlives runs and upgrades — including
daemons from stale `dist/` binaries. A loopback rig showed the `no_proxy`
bypass still live through a reused pre-fix daemon. A second instance of the
same pattern: children (mpv, private tor) share the parent's process
group/console, so signal semantics claimed in the docs ("Ctrl+C never kills the
daemon", "Ctrl+C orphans tor") were both wrong when measured.

**Why:** the docs and tests in this project pin the spawn (argv/env stand-ins)
and reason about the parent process alone; nobody is assigned to test reuse or
group-signal delivery.

**How to apply:** whenever a fix touches `Ensure()`, `spawnPrivateTor`, or any
child spawn, ask (1) what happens when an older/foreign instance is already
running and gets reused, (2) what the child receives when the terminal sends
SIGINT/SIGHUP to the group or the Windows console sends CTRL_C/CLOSE. Both
are measurable offline (see [[project-measurement-environment]]).

Related: [[project-house-style-measured-claims]]
