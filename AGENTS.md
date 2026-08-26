# Plex STRM Proxy Maintenance Guide

This file is written for AI agents and maintainers working on the public `plex-strm-proxy` repository.

## Project boundaries

This is a Go reverse proxy in front of Plex Media Server. It reads `.strm` files from Plex media directories, maps their HTTP(S) source URLs to Plex `part_id` values for a short time in memory, and applies a playback policy when the client starts playback.

The following constraints must remain true:

- Do not modify the Plex database.
- Keep ordinary Plex API, XML/JSON unknown fields, Range/HEAD handling, and `X-Plex-*` requests transparent.
- Do not hard-code Android, Plex Web, a user agent, or a device model.
- Prefer direct STRM playback, but do not force ordinary Plex media through this proxy's transcoder.
- Never forward Plex tokens or authentication headers to third-party media hosts.
- Do not introduce Caddy as a project dependency.

## Request and playback architecture

- `cmd/plex-strm-proxy/main.go`: process entry point and HTTP server startup.
- `internal/app/server.go`: routing, transparent Plex proxying, STRM mappings, and request orchestration. Do not duplicate codec or `directPlay` decisions here.
- `internal/app/resolver.go`: safe STRM file reading and parsing.
- `internal/app/mapping.go`: TTL mapping from structured Plex responses to `part_id -> PartMapping`.
- `internal/app/playback_policy.go`: the single playback-plan entry point. Extend and test the pure `SelectPlaybackPlan` function first.
- `internal/app/target.go`: URL scheme, port, private-address, DNS, and redirect validation, plus the policy-bound media HTTP client.
- `internal/app/media_probe.go`: FFprobe-based codec, duration, and color metadata probing. Probing must go through `ResolveMediaTarget` first.
- `internal/app/hls_transcode.go`: on-demand HLS compatibility fallback for STRM only; it is not the ordinary Plex transcoder.
- `internal/app/metadata*.go` and `decision*.go`: preserve Plex XML/JSON structures and change only the fields that must be rewritten.

The playback plans have these ownership boundaries:

| Plan | Owner | Typical condition |
| --- | --- | --- |
| `strm-redirect` | Client and external media source | STRM direct-first, explicit `directPlay=1`, or confirmed Direct Play |
| `plex-transcode` | Plex | Ordinary media transcoding, HLS disabled, or unsafe proxy fallback |
| `proxy-hls-audio-fallback` | FFmpeg inside this proxy | Incompatible STRM audio such as PCM, AC3, DTS, E-AC-3, or MLP/TrueHD, without an explicit Direct Play request |
| `proxy-hls-video-fallback` | FFmpeg inside this proxy | STRM has entered HLS start and Plex cannot reliably take over the STRM path |

Proxy HLS first attempts `video copy + audio AAC`; it falls back to H.264/AAC video transcoding if stream copy fails. HLS fallback consumes NAS CPU, temporary disk space, and network bandwidth, so it must not become the default path for all media.

### Native STRM coordination details

The public README intentionally describes outcomes and configuration rather than this
request choreography. Keep the following details here when changing the implementation:

- Home and library-list metadata responses must not cold-probe every remote STRM source.
  A detailed metadata request may perform one bounded probe for the selected item and
  reuse the result from the in-memory probe cache. A play queue may cold-probe only its
  selected STRM Part. Ordinary Plex media must not enter this probe path.
- Before FFprobe or FFmpeg opens a remote STRM URL, call `ResolveMediaTarget` through the
  policy-bound client. It follows and validates the redirect chain, first trying a
  bounded `HEAD` with `Range: bytes=0-0` and retrying with a bounded range `GET` when the
  source times out, rejects `HEAD`, or sends `HEAD` to a login page. The selected detail
  probe has a separate 45-second budget; list and home requests still do not probe.
- Native STRM decision requests use a proxy-reachable metadata/Part callback so Plex can
  make the native Direct Play, Direct Stream, or transcode decision. Only a native
  Direct Play result may have the selected Part file rewritten to the validated source
  URL and remembered for the current playback session and Part. A selected Part/Stream
  transcode decision must remain Plex-owned and must not populate the direct cache.
  Later requests that explicitly set both `directPlay=0` and `directStream=0` must always
  be passed back to Plex, even if an earlier request in the same session was Direct Play.
- A client cancellation or timeout is not evidence that Plex rejected the native STRM
  path. Proxy FFmpeg/HLS may start only for an actual rejected STRM decision/start and
  an enabled fallback. This fallback is STRM-only; ordinary Plex media remains on the
  transparent Plex path. HLS clients receive HLS playlists, while DASH clients receive
  a DASH MPD and fragmented MP4 segments.
- `PLEX_CALLBACK_URL` is the address Plex uses to call back into the proxy for metadata
  and Part requests. The proxy's own listener address remains the address used by the
  in-process FFprobe/FFmpeg clients. In bridge networking or with different host and
  container ports, validate this two-address topology with a real Plex-to-proxy request.

## Outbound security rules

Every remote request originating from an STRM file must use `TargetPolicy` and `NewMediaClient`. Do not create a bare `http.Client`, and do not pass an unvalidated STRM URL directly to FFprobe or FFmpeg.

The default policy allows HTTP/HTTPS only, permits ports 80/443, and rejects localhost, private, link-local, unspecified, and multicast addresses. Signed URL query strings must be preserved exactly. Plex tokens may be used for the Plex upstream only and must never be appended to third-party media URLs.

`ResolveMediaTarget` performs a small `Range: bytes=0-0` request with the policy-bound client, follows and validates the redirect chain, and returns the final URL before a subprocess or client receives it. The FFmpeg/FFprobe versions currently used do not expose one consistently usable `max_redirects` command-line option; do not add that flag again without first validating it against the actual binaries and adding a testable boundary.

HLS long seeks may create temporary source-file caches below `TRANSCODE_ROOT`. This is not a full media-library cache, but it can consume significant disk, network, and process resources. Changes to caching, concurrency, or session lifetime must include resource-reclamation tests and a real seek test.

## Unraid and remote Docker

Some deployments may use an SSH tunnel to expose a Docker socket locally. The tunnel must be opened by the operator; an AI agent must not create or assume a private tunnel:

```bash
export DOCKER_HOST=tcp://127.0.0.1:2375
ssh -nNT <operator-configured-tunnel> &
```

If Docker reports `connection refused`, ask the operator to open the configured tunnel. Do not use Swarm, Secrets, or Docker Auth operations when a socket proxy is in place.

A generic host-network deployment looks like this; real paths and upstream addresses belong in the operator's private deployment configuration, not in this repository:

```text
--network host
-e PLEX_UPSTREAM=http://127.0.0.1:32400
-e LISTEN_ADDR=0.0.0.0:3001
-e STRM_ROOTS=/MediaA,/MediaB
-e PLAYBACK_MODE=redirect
-e REDIRECT_STATUS=302
-e STRM_HLS_TRANSCODE=true
-e STRM_HLS_COPY_FIRST=true
-e PROXY_FALLBACK=false
-v /path/to/strm-a:/MediaA:ro
-v /path/to/strm-b:/MediaB:ro
```

Before replacing a running container, inspect and preserve its environment variables and read-only mounts. Replace the image without silently changing the media roots or making them writable.

## Development and verification

The host may not have Go installed. Prefer a disposable Go builder container when necessary. With a Go toolchain available, run at least:

```bash
gofmt -w cmd internal
go test ./...
go test -race ./...
```

ADB regression should cover:

1. A direct-play media sample: Android MediaSession reaches `PLAYING`, the logs show the STRM direct/Part path, and no proxy FFmpeg process is created.
2. A PCM audio sample: the logs show `proxy-hls-audio-fallback` and `video-copy-audio-transcode`.
3. An E-AC-3 or TrueHD sample: HLS reaches `PLAYING` and remains stable for at least 20--60 seconds.
4. A container/codec combination that is not directly playable: verify that ordinary Plex transcoding is not accidentally rewritten as proxy HLS; if Plex requests STRM HLS, confirm that it is an explicit STRM fallback.

Record `adb shell dumpsys media_session`, proxy logs, `docker stats`, and Plex Transcoder state together. `scrcpy --turn-screen-off --stay-awake` may be used to keep the device awake while turning off the display; it must not lock the device.

## Change principles

- Prefer changes to the centralized playback policy, outbound security, and testable boundaries over large route-specific rewrites.
- Do not turn a device model, user agent, or media title into a special case.
- Do not globally force Direct Play because one client fails.
- Preserve unknown Plex XML/JSON fields and original encoding when rewriting responses.
- When changing HLS sessions, process termination, redirects, or source caching, check locks, `processDone`, response-body closure, temporary-file cleanup, and idle-session cleanup.
- Update `README.md` when observable behavior or configuration changes.
- Keep private deployment addresses, tokens, media paths, and local orchestration state out of the public repository.
