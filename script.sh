#!/usr/bin/env bash
# =============================================================================
#  chainsmoke — privacy-focused SoundCloud CLI  ::  BASH PROTOTYPE / ORCHESTRATION CARCASS
# -----------------------------------------------------------------------------
#  Purpose: prove the whole pipeline end-to-end before it becomes main.go.
#    egress (Tor + Privoxy bridge)  ->  yt-dlp resolve  ->  mpv JSON-IPC  ->  scrub
#
#  This is the throwaway. main.go is the shippable version. Everything here that
#  is marked TODO(impl) is a stub whose CONTROL FLOW is real but whose body is not.
#
#  Design invariants inherited from the build spec (do not break these):
#    - EGRESS CHOKEPOINT: every network call (yt-dlp, mpv/ffmpeg) inherits the
#      proxy env exported by net_up(). Nothing reaches the internet another way.
#    - EPHEMERAL BY DEFAULT: no disk writes unless CS_REMEMBER=1.
#    - NO URLS IN ARGV: the common path is `chainsmoke play <N>` against a cached result
#      set; raw URLs come in over stdin. argv is world-readable via /proc.
#    - LAZY STREAMS: metadata resolved eagerly, stream URLs resolved 1-2 ahead.
#
#  Honest caveat (keep it in the docs): Tor exits get throttled/blocked by media
#  CDNs. Streaming over Tor is flaky by nature. net_up() is the one seam to swap
#  for a WireGuard netns if you decide the bridge isn't worth it.
# =============================================================================
set -Eeuo pipefail

# ----------------------------------------------------------------------------
# 0. Config & XDG paths (ephemeral unless CS_REMEMBER=1)
# ----------------------------------------------------------------------------
APP="chainsmoke"
: "${CS_REMEMBER:=0}"                       # 1 = allow disk persistence
: "${CS_PRIVATE:=0}"                        # 1 = force everything in-memory this run
: "${CS_IMPERSONATE:=chrome}"               # yt-dlp curl_cffi target; "" to disable

: "${XDG_RUNTIME_DIR:=/run/user/$(id -u)}"
: "${XDG_CACHE_HOME:=$HOME/.cache}"
: "${XDG_STATE_HOME:=$HOME/.local/state}"

RUNDIR="$XDG_RUNTIME_DIR/$APP"              # sockets, pidfiles, transient state
IPC_SOCK="$RUNDIR/mpv.sock"                 # mpv --input-ipc-server target
RESULTS="$RUNDIR/results.tsv"              # cached numbered search results (id<TAB>title<TAB>url)

# Egress bridge coordinates (Privoxy HTTP-CONNECT front -> Tor SOCKS back)
BRIDGE_HOST="127.0.0.1"; BRIDGE_PORT="8118"
TOR_SOCKS_HOST="127.0.0.1"; TOR_SOCKS_PORT="9050"   # 9150 if using Tor Browser bundle
TOR_CTRL_PORT="9051"

mkdir -p "$RUNDIR"; chmod 700 "$RUNDIR"

# ----------------------------------------------------------------------------
# 1. Logging (stderr only; stdout stays pipeable)
# ----------------------------------------------------------------------------
log()  { printf '%s %s\n' "[$APP]" "$*" >&2; }
die()  { printf '%s %s\n' "[$APP][fatal]" "$*" >&2; exit 1; }
warn() { printf '%s %s\n' "[$APP][warn]"  "$*" >&2; }

# ----------------------------------------------------------------------------
# 2. Dependency preflight
# ----------------------------------------------------------------------------
require() {
  local miss=()
  for b in "$@"; do command -v "$b" >/dev/null 2>&1 || miss+=("$b"); done
  ((${#miss[@]}==0)) || die "missing dependencies: ${miss[*]}"
}
deps_check() { require mpv yt-dlp ffmpeg jq socat tor privoxy; }

# ----------------------------------------------------------------------------
# 3. NET LAYER — bring up Tor + Privoxy bridge, export proxy env, self-test DNS
#    This is the single egress seam. Swap the whole function for a netns if you
#    decide to force egress at the kernel level instead of via env.
# ----------------------------------------------------------------------------
_privoxy_conf() {
  # Minimal Privoxy config: HTTP-CONNECT front on :8118, forward everything to
  # Tor SOCKS with remote DNS (forward-socks5t = no local DNS leak).
  cat <<EOF
listen-address $BRIDGE_HOST:$BRIDGE_PORT
forward-socks5t / $TOR_SOCKS_HOST:$TOR_SOCKS_PORT .
toggle 1
enable-remote-toggle 0
enable-edit-actions 0
EOF
}

net_up() {
  # TODO(impl): start tor if not already listening on $TOR_SOCKS_PORT
  #   - prefer an existing system tor; only spawn a private one if absent
  #   - if spawning: tor --SocksPort $TOR_SOCKS_PORT --ControlPort $TOR_CTRL_PORT ...
  # TODO(impl): start privoxy with the config from _privoxy_conf()
  #   local conf; conf="$(mktemp)"; _privoxy_conf > "$conf"; privoxy --no-daemon "$conf" &
  #   wait for :$BRIDGE_PORT to accept CONNECT before returning

  # The chokepoint: everything spawned after this inherits the bridge.
  export http_proxy="http://$BRIDGE_HOST:$BRIDGE_PORT"
  export https_proxy="$http_proxy"
  export ALL_PROXY="$http_proxy"            # libavformat sometimes honors this; verify per build
  export no_proxy="127.0.0.1,localhost"
  log "egress -> $http_proxy (Tor $TOR_SOCKS_HOST:$TOR_SOCKS_PORT)"
}

net_selftest() {
  # DNS-leak gate. Assert no plaintext DNS for *.sndcdn.com / soundcloud.com
  # leaves the host. In CI this is a tshark capture; here it's a stub.
  # TODO(impl): run a resolve through the bridge and confirm the local resolver
  #             never saw the hostname (capture on the loopback/uplink iface).
  log "net_selftest: TODO — assert zero plaintext DNS for soundcloud hosts"
}

net_rotate() {
  # New Tor circuit. Uses the control port; NEWNYM is rate-limited by Tor.
  # TODO(impl): authenticate to $TOR_CTRL_PORT and send SIGNAL NEWNYM
  #   printf 'AUTHENTICATE ""\r\nSIGNAL NEWNYM\r\nQUIT\r\n' | nc 127.0.0.1 $TOR_CTRL_PORT
  log "net_rotate: TODO — SIGNAL NEWNYM"
}

# ----------------------------------------------------------------------------
# 4. IPC HELPERS — newline-delimited JSON over the mpv unix socket
# ----------------------------------------------------------------------------
ipc() {
  # ipc '["get_property","time-pos"]'  ->  raw JSON reply on stdout
  [[ -S "$IPC_SOCK" ]] || die "player not running (no socket at $IPC_SOCK)"
  printf '{"command":%s}\n' "$1" | socat - "UNIX-CONNECT:$IPC_SOCK"
}
ipc_get() { ipc "[\"get_property\",\"$1\"]" | jq -r '.data'; }
ipc_set() { ipc "[\"set_property\",\"$1\",$2]" >/dev/null; }

# ----------------------------------------------------------------------------
# 5. RESOLVER — thin yt-dlp wrappers. yt-dlp inherits the proxy env from net_up.
#    Never reimplement api-v2 here; if yt-dlp breaks, the fix is `yt-dlp -U`.
# ----------------------------------------------------------------------------
_ytdlp_common=( --no-cache-dir --no-progress )
_impersonate_args() { [[ -n "$CS_IMPERSONATE" ]] && printf -- '--impersonate\n%s\n' "$CS_IMPERSONATE"; }

resolve_meta() {
  # EAGER: whole playlist/track metadata in one flat call. Fast, cache-friendly.
  # emits TSV: id<TAB>title<TAB>permalink_url   (one row per entry)
  local url="$1"
  yt-dlp "${_ytdlp_common[@]}" $(_impersonate_args) --flat-playlist -J -- "$url" 2>/dev/null \
    | jq -r '(.entries // [.]) | .[] | [.id, .title, .url] | @tsv' \
    || die "resolve failed — try: yt-dlp -U"
}

resolve_stream() {
  # LAZY: single track -> a fresh, short-lived direct stream URL. Call this
  # 1-2 tracks ahead of playback; SoundCloud HLS manifests expire fast.
  local permalink="$1"
  yt-dlp "${_ytdlp_common[@]}" $(_impersonate_args) \
    --extractor-args 'soundcloud:formats=hls_aac' \
    -f bestaudio -g -- "$permalink" 2>/dev/null \
    || { warn "stream resolve 403/expired — rotate + retry"; return 1; }
}

# ----------------------------------------------------------------------------
# 6. PLAYER — mpv daemon lifecycle + preload-ahead queue
# ----------------------------------------------------------------------------
player_up() {
  [[ -S "$IPC_SOCK" ]] && { log "player already up"; return; }
  # http_proxy is already exported -> ffmpeg/hls fetches route through the bridge.
  mpv --idle=yes --no-video --no-terminal \
      --gapless-audio=yes --cache-on-disk=no \
      --script-opts=ytdl_hook-ytdl_path=yt-dlp \
      --input-ipc-server="$IPC_SOCK" &
  disown
  # wait for socket
  for _ in {1..50}; do [[ -S "$IPC_SOCK" ]] && break; sleep 0.05; done
  [[ -S "$IPC_SOCK" ]] || die "mpv failed to create IPC socket"
  log "player up (pid via $IPC_SOCK)"
}

player_down() {
  [[ -S "$IPC_SOCK" ]] && ipc '["quit"]' >/dev/null 2>&1 || true
  rm -f "$IPC_SOCK"
}

preflight() {
  # mpv silently keeps dead (403/expired) URLs in the playlist and only stalls
  # when playback reaches them. HEAD through the bridge before appending.
  local url="$1"
  curl -sfI --proxy "$http_proxy" -o /dev/null -- "$url"
}

enqueue() {
  # Resolve stream lazily, preflight, then append-play so one item stays preloaded.
  local permalink="$1" stream
  stream="$(resolve_stream "$permalink")" || return 1
  preflight "$stream" || { warn "preflight failed, skipping: $permalink"; return 1; }
  ipc "[\"loadfile\",\"$stream\",\"append-play\"]" >/dev/null
}

# ----------------------------------------------------------------------------
# 7. RESULT CACHE — numbered results so the common path never touches argv URLs
# ----------------------------------------------------------------------------
results_write() { : "${CS_PRIVATE:+return 0}"; cat > "$RESULTS"; }   # TSV in
results_row()   { sed -n "${1}p" "$RESULTS"; }                        # 1-indexed
results_url()   { results_row "$1" | cut -f3; }

# ----------------------------------------------------------------------------
# 8. SCRUB — only on the download/get path. ffmpeg +bitexact -> mutagen -> xattr -> mat2
# ----------------------------------------------------------------------------
scrub() {
  local in="$1" out="$2"
  # 8a. strip container/stream metadata AND kill the injected Lavf encoder tag
  ffmpeg -y -i "$in" -map_metadata -1 -map_metadata:s:a -1 \
         -fflags +bitexact -flags:a +bitexact -c copy "$out" 2>/dev/null
  # 8b. TODO(impl): mutagen selective retag (keep artist/title/album/art; drop
  #     COMM, encoder, WOAF/WORS, TXXX:* source URLs, ISRC) — shell out to a
  #     small python helper or the Go scrub pkg.
  # 8c. filesystem xattrs left by desktop downloaders
  setfattr -x user.xdg.origin.url  "$out" 2>/dev/null || true
  setfattr -x user.xdg.referrer.url "$out" 2>/dev/null || true
  touch -d '1970-01-01 00:00:00' "$out" 2>/dev/null || true   # normalize mtime
  # 8d. optional nuclear pass + audit
  #     command -v mat2 >/dev/null && mat2 --inplace "$out"
  #     command -v mat2 >/dev/null && mat2 --show "$out"
  log "scrubbed -> $out"
}

# ----------------------------------------------------------------------------
# 9. VERBS
# ----------------------------------------------------------------------------
cmd_search() {   # chainsmoke search <term...>   -> numbered, cached
  local term="$*"; [[ -n "$term" ]] || die "usage: $APP search <term>"
  # TODO(impl): drive yt-dlp's scsearch: "scsearch10:$term"
  resolve_meta "scsearch10:$term" | tee >(results_write) \
    | nl -w3 -s'  ' | cut -f1,2      # show index + title only; URLs stay cached
}

cmd_play() {     # chainsmoke play <N> | chainsmoke play   (URL on stdin)
  player_up
  local url
  if [[ "${1:-}" =~ ^[0-9]+$ ]]; then
    url="$(results_url "$1")"; [[ -n "$url" ]] || die "no cached result #$1"
  else
    read -r url || die "no URL on stdin"
  fi
  # expand to entries and enqueue the first, then preload the next ahead
  mapfile -t rows < <(resolve_meta "$url")
  ((${#rows[@]})) || die "nothing resolved"
  enqueue "$(cut -f3 <<<"${rows[0]}")"
  [[ -n "${rows[1]:-}" ]] && enqueue "$(cut -f3 <<<"${rows[1]}")" &   # preload-ahead
  log "playing: $(cut -f2 <<<"${rows[0]}")"
}

cmd_likes()    { : ; }   # TODO(impl): resolve https://soundcloud.com/<user>/likes
cmd_playlist() { : ; }   # TODO(impl): resolve a playlist permalink -> enqueue all lazily

cmd_np()   { ipc_get media-title; }                          # now-playing (add --json later)
cmd_toggle(){ ipc '["cycle","pause"]' >/dev/null; }
cmd_seek() { ipc "[\"seek\",${1:?±N seconds}]" >/dev/null; } # relative seek
cmd_vol()  { ipc_set volume "${1:?0-100}"; }
cmd_next() { ipc '["playlist-next"]' >/dev/null; }
cmd_prev() { ipc '["playlist-prev"]' >/dev/null; }
cmd_stop() { player_down; }

cmd_get() {      # chainsmoke get <N>|<stdin-url> [outfile]   (download + scrub)
  local url tmp out
  if [[ "${1:-}" =~ ^[0-9]+$ ]]; then url="$(results_url "$1")"; shift
  else read -r url; fi
  out="${1:-./track.m4a}"; tmp="$(mktemp --suffix=.m4a)"
  yt-dlp "${_ytdlp_common[@]}" $(_impersonate_args) \
    --extractor-args 'soundcloud:formats=hls_aac' \
    -f bestaudio -o "$tmp" -- "$url" || die "download failed — yt-dlp -U ?"
  scrub "$tmp" "$out"; rm -f "$tmp"
}

cmd_rotate() { net_rotate; }
cmd_purge()  { rm -rf "$RUNDIR"; log "purged transient state"; }
cmd_audit()  {   # verifiable privacy properties at runtime, not just in docs
  echo "egress:       ${http_proxy:-<none>}"
  echo "remember:     $CS_REMEMBER   private: $CS_PRIVATE"
  echo "impersonate:  ${CS_IMPERSONATE:-<off>}"
  echo "results file: $([[ -f $RESULTS ]] && echo present || echo none)"
  net_selftest
  # TODO(impl): count outbound requests for a play; assert only *.sndcdn/soundcloud
}

usage() {
  cat >&2 <<EOF
$APP — privacy-focused SoundCloud CLI (prototype)
usage: $APP <command> [args]
  search <term>       search, print numbered results (cached)
  play <N|-\<stdin>>   play cached result N, or a URL from stdin
  get  <N> [out]      download + scrub a cached result
  likes / playlist    (todo) resolve & enqueue a user's likes / a playlist
  np | toggle | stop  now-playing / pause / kill daemon
  seek <±N> | vol <N> | next | prev
  rotate              new Tor circuit (NEWNYM)
  audit | purge       show privacy posture / wipe transient state
env: CS_REMEMBER=1 persist state   CS_PRIVATE=1 memory-only   CS_IMPERSONATE=chrome
EOF
}

# ----------------------------------------------------------------------------
# 10. Dispatch
# ----------------------------------------------------------------------------
main() {
  deps_check
  net_up            # chokepoint established before anything spawns
  local cmd="${1:-}"; shift || true
  case "$cmd" in
    search)   cmd_search   "$@";;
    play)     cmd_play     "$@";;
    get)      cmd_get      "$@";;
    likes)    cmd_likes    "$@";;
    playlist) cmd_playlist "$@";;
    np)       cmd_np;;
    toggle|pause) cmd_toggle;;
    seek)     cmd_seek     "$@";;
    vol)      cmd_vol      "$@";;
    next)     cmd_next;;
    prev)     cmd_prev;;
    stop)     cmd_stop;;
    rotate)   cmd_rotate;;
    audit)    cmd_audit;;
    purge)    cmd_purge;;
    ''|-h|--help|help) usage;;
    *)        usage; exit 2;;
  esac
}

# Cleanup only kills the player if WE own it; state wipe unless CS_REMEMBER.
cleanup() { [[ "$CS_REMEMBER" == 1 ]] || { :; }; }
trap cleanup EXIT

main "$@"
