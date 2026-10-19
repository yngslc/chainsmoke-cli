# chainsmoke

A command-line SoundCloud player that sends everything over Tor.

chainsmoke searches SoundCloud, lists a user's likes or a playlist, and plays
tracks in mpv. Every request goes through Tor: the yt-dlp lookups, the stream
check, and the audio itself. If chainsmoke cannot prove that path works, it
refuses to run rather than fall back to your own IP.

It is a single static Go binary with one direct dependency (urfave/cli).
There is no account, no login and no API key.

## Requirements

- **mpv**. Tested with mpv 0.41.
- **yt-dlp** with curl_cffi, for browser TLS impersonation:
  `pip install "yt-dlp[default,curl-cffi]"`. Without curl_cffi, pass
  `--impersonate ''`.
- **Tor**, as one of these:
  - a running tor with its SOCKS port on `127.0.0.1:9050`;
  - `tor` on your PATH, in which case chainsmoke starts a private tor for the
    run and stops it afterwards;
  - the Tor Browser, with `--tor-socks 127.0.0.1:9150`.
- **Go 1.26.5 or newer**, to build it.

chainsmoke is developed and tested on Windows 10 and on Ubuntu under WSL. The
macOS builds compile but have not been tested.

## Build

```sh
go build -trimpath -o chainsmoke ./cmd/chainsmoke
```

`build.ps1 -Version X -All` cross-builds release binaries for Windows, Linux
and macOS (amd64 and arm64) into `dist/`.

## Usage

```sh
chainsmoke search late night drive     # numbered results
chainsmoke play 3                      # play result 3; the rest of the list follows
chainsmoke likes someuser              # a user's public likes, numbered
chainsmoke playlist someuser/some-set  # a set: the tail of soundcloud.com/<user>/sets/<set>
echo "https://soundcloud.com/artist/track" | chainsmoke play
```

URLs are never accepted as arguments, because any user on the machine can read
a process's arguments. Pipe a URL in, or run `play` with no argument and paste
the URL at the prompt.

### The play session

`play` keeps running while you listen, because the Tor bridge that mpv streams
through lives inside that process. On a terminal it becomes a `chainsmoke>`
prompt:

| Command | What it does |
|---|---|
| `next`, `prev` | Move along the list: the search results, likes or set that `play` started from |
| `shuffle [on\|off]` | Play the rest of the list in random order |
| `repeat one\|all\|off` | Repeat one track, or start the list over when it ends |
| `search …`, `likes …`, `playlist …` | List new results; `play N` then switches to them |
| `play N` | Play result N of the current results, and the rest of that list after it |
| `rotate` | Fresh Tor circuit for the next stream |
| `np`, `toggle`, `pause`, `seek ±N`, `vol 0-100` | The usual transport controls |
| `q` | Stop playback and exit |

The next track always resolves in the background, so `next` starts at once.
When a track ends by itself, the prompt says what is playing now and what
comes next.

With a URL piped in, there is no prompt. The session plays to the end of the
track or set, and Ctrl+C stops it.

### From another terminal

`np`, `toggle`, `pause`, `seek`, `vol`, `next`, `prev` and `stop` also work as
one-shot commands while a session is playing. From outside the session,
`next` and `prev` only move within what mpv already has queued. To walk the
whole list, use the session's prompt.

### Other commands

- `audit`: shows the current privacy posture (the egress path, whether it was
  verified, and what local DNS can observe). `audit --save` records this setup
  as the one you expect, and later audits report anything that differs from
  it.
- `rotate`: gives the next stream a fresh Tor circuit by switching the
  bridge's SOCKS credentials, and also sends NEWNYM when tor has a
  ControlPort. Run it at the session prompt. The credentials belong to the
  process that holds the bridge, so from another terminal only NEWNYM can
  reach a playing session.
- `purge`: stops the player and wipes every stored result and history.
- `daemon start|stop|status`: manages the mpv daemon.

## Privacy model

- **Fail closed.** No lookup and no playback runs until a real connection has
  gone through bridge → Tor → exit in that same process. `--allow-direct` is
  the only way to run without Tor. It must be passed explicitly, and it prints
  a warning every time.
- **Remote DNS.** Hostnames are handed to Tor as text and never resolved
  locally.
- **No URLs in arguments or output.** Track URLs never go on a command line.
  The signed stream URLs, which work as credentials, are never printed or
  logged.
- **Ephemeral by default.** Results live in a private (0700) runtime
  directory: `$XDG_RUNTIME_DIR/chainsmoke`, or `/run/user/<uid>/chainsmoke`,
  or `%TEMP%\chainsmoke` on Windows. On Linux that is usually tmpfs and gone
  at logout. On Windows `%TEMP%` is ordinary disk and survives a reboot, so
  `purge` is what removes it there. `--remember` keeps history on disk, and
  `purge` removes both.
- **Isolated circuits.** chainsmoke's traffic gets its own Tor circuits
  (SOCKS isolation), separate from anything else using the same tor.

## Known limitations

- **Tor exits and SoundCloud's CDN don't get along.** Exits are often
  throttled or blocked, so expect retries, buffering and the occasional
  `rotate`.
- **The bridge has no authentication, and a play session keeps it open.** It
  listens on a loopback port. Until you quit, any local process that finds the
  port can tunnel through it on your Tor circuits. The bridge only carries
  encrypted tunnels (CONNECT); it relays no plain HTTP.
- **One proxy path has not been measured end to end.** A loopback test showed
  HLS playlists and segments going through the proxy, and failing closed when
  the proxy dies. HTTPS segments through the real bridge to the real CDN have
  not been measured.
- **The stream URL is visible to the mpv daemon.** It sits in mpv's playlist,
  so anything that can reach mpv's IPC endpoint can read it.
- **Using Tor is not hidden.** Your ISP can see that you use Tor, and the
  timing and volume of an audio stream are distinctive.
- **`audit`'s DNS check sends one plaintext query** for `example.com`.
- **Major-label DRM tracks don't play.**
- **Downloading isn't implemented yet.**

## License

MIT; see [LICENSE](LICENSE). Third-party notices, including the code adapted
from nao1215/tornago, are in [NOTICE](NOTICE).

chainsmoke accesses SoundCloud's public web interface anonymously. Respect
SoundCloud's terms and the rights of the artists you listen to.
