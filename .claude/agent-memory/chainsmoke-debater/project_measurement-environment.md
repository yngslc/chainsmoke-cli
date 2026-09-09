---
name: project-measurement-environment
description: What this box can actually measure — WSL has gcc so `go test -race` runs locally, contradicting the docs; loopback rigs are the safe way to test egress.
metadata:
  type: project
---

Two environment facts that the project docs get wrong or do not state, both
verified 2026-09-09.

- **`go test -race` runs locally.** SKILL.md and `.github/workflows/ci.yml` both
  say the race detector "has no home except CI" because the Windows box lacks
  cgo. True of Windows, false of the box: WSL (Ubuntu-26.04) carries `go`,
  `gcc` and `cc`. `wsl -e bash -c "cd /mnt/d/sccli && go test ./... -race -count=1"`
  works and was green. Use it rather than deferring concurrency questions to CI.
- **The GitHub remote is private and unverifiable from here.** `origin` is
  `https://github.com/yngslc/Chainsmoke-CLI.git`; unauthenticated `api.github.com`
  returns 404 and `gh` is installed neither on Windows nor in WSL. So no agent in
  this loop can confirm a CI run ever went green — treat "CI is green" as
  unverified, not as fact.

**Why:** privacy findings in this project are settled by breaking something and
watching, not by reasoning. Knowing which measurements are actually available
here decides whether a claim gets a counter-measurement or a MEASUREMENT-tier
filing.

**How to apply:** to test egress behaviour without leaving the box, build a
scratch Go rig under the session scratchpad: a `net.Listen("tcp","127.0.0.1:0")`
standing in for the origin, the four proxy env spellings pointed at
`http://127.0.0.1:1`, and the subprocess spawned exactly as the tree spawns it.
That pattern reproduced the `mpv` `cmd.Env` leak over both HTTP and HTTPS and
settled whether `--stream-lavf-o=http_proxy` reaches HLS child segments. Never
add a smoke case that performs real egress, and never run `-tags integration`.

Related: [[project-house-style-measured-claims]]
