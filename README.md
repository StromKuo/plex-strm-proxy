# plex-strm-proxy

[English](README.md) · [简体中文](README.zh-CN.md)

A Go reverse proxy that helps Plex play media referenced by `.strm` files. It is designed for Plex Web, app.plex.tv, and official Plex clients.

The proxy leaves Plex's library and database unchanged.

## How it works

For an STRM item, the proxy prefers to redirect the client to the real HTTP(S) media URL. This allows the client and the media provider to exchange media bytes directly, without sending the whole file through Plex or the NAS.

When a client cannot play the source directly, an optional HLS compatibility fallback can convert STRM playback into a format the client can handle. This uses FFmpeg and applies only to STRM media; ordinary Plex media continues to use Plex's own playback and transcoding pipeline.

## Requirements

- Plex Media Server with `.strm` items already visible in the library.
- Read-only access inside the container to the directories containing those `.strm` files.
- A media URL that the playback client can reach when redirect mode is used.
- HTTP Range support from the media provider is strongly recommended. Range requests allow seeking and partial reads.
- FFmpeg is required only for the optional STRM HLS fallback.

## Quick start

The published image is available from GHCR. It includes FFmpeg for the optional STRM HLS fallback; redirect mode does not invoke FFmpeg.

```bash
docker pull ghcr.io/stromkuo/plex-strm-proxy:latest
```

Use a version tag such as `1.0.0` when you want to pin a release instead of following `latest`.

```bash
docker run --rm \
  --name plex-strm-proxy \
  -p 3001:3001 \
  -e PLEX_UPSTREAM=http://plex:32400 \
  -e STRM_ROOT=/media \
  -v /path/to/plex/media:/media:ro \
  ghcr.io/stromkuo/plex-strm-proxy:latest
```

To build locally instead of pulling the published image:

```bash
docker build -t plex-strm-proxy .
```

In the volume mapping, the path on the left is on the host and the path on the right is inside the container. The proxy-side path must match the path Plex uses for the same `.strm` files and the configured `STRM_ROOT`.

For example, if the Plex container uses:

```bash
-v /host/media:/media
```

the proxy container should use:

```bash
-v /host/media:/media:ro
-e STRM_ROOT=/media
```

For multiple STRM roots, use `STRM_ROOTS` instead of `STRM_ROOT`:

```bash
-e STRM_ROOTS=/Movies,/TV
-v /path/to/movies:/Movies:ro
-v /path/to/tv:/TV:ro
```

## Plex setup

Add the client-accessible proxy URL to Plex Server Settings → Network → `Custom server access URLs`.

Examples:

```text
http://plex-proxy.example.com:3001
https://plex-proxy.example.com/
```

For public access, use an HTTPS URL with a valid certificate. An external reverse proxy or tunnel can provide HTTPS.

After changing the URL, sign out and back in to the client if it continues using an old server connection. Test a normal Plex item as well as an STRM item.

### Choosing the connection path

On the LAN, a direct Plex connection normally uses the Plex server's LAN address on port `32400`; this proxy listens on port `3001`. A client that connects directly to the Plex server on `32400` bypasses the proxy. To test or use the proxy on the LAN, make the client reach the Plex server's LAN address on `3001` or a LAN DNS name that points to the proxy.

For public use in the official app or app.plex.tv, configure a publicly reachable Custom server access URL, preferably an HTTPS domain with a valid certificate. For this project's public proxy setup, disable Plex `Remote Access` so Plex does not also publish its direct public `32400` endpoint.

## Common settings

| Variable | Default | Purpose |
| --- | --- | --- |
| `PLEX_UPSTREAM` | `http://plex:32400` | Plex Media Server address |
| `LISTEN_ADDR` | `0.0.0.0:3001` | Proxy listen address |
| `STRM_ROOT` / `STRM_ROOTS` | `/media` | Read-only STRM root(s) |
| `PLAYBACK_MODE` | `redirect` | `redirect` sends clients to the source URL; `proxy` streams through this service |
| `STRM_HLS_TRANSCODE` | `true` | Enable STRM HLS compatibility fallback |

For security, HTTP/HTTPS media URLs on standard ports are allowed by default. Private network targets and non-standard ports are blocked unless explicitly enabled.

## Limitations

- Redirect mode requires the client to reach the external media URL itself.
- If a media URL contains a signature or expiration parameter, it must remain valid for the entire playback session, and the media provider must support partial reads and seeking (HTTP Range).
- HLS fallback uses proxy CPU, temporary disk space, and network bandwidth.
- Long seeks in HLS fallback may temporarily use additional disk space and network bandwidth.
- Compatibility still depends on the codecs, container, audio format, and subtitle behavior supported by the client.

## Troubleshooting

Follow the proxy logs while reproducing the problem:

```bash
docker logs -f plex-strm-proxy
```

Check that:

1. The client can reach the URL configured in `Custom server access URLs`.
2. The client can reach the real URL stored in the STRM file.
3. The media provider supports Range requests.
4. The Plex server and proxy can reach each other.
5. A normal Plex item still plays, which helps separate Plex connectivity issues from STRM compatibility issues.

## Development

```bash
go test ./...
```

GitHub Actions runs the tests and a Docker build for pushes and pull requests. A published GitHub Release with a tag such as `v1.0.0` builds and publishes multi-platform images to GHCR:

```text
ghcr.io/stromkuo/plex-strm-proxy:1.0.0
ghcr.io/stromkuo/plex-strm-proxy:latest
```

The `latest` tag is updated only for non-prerelease versions. The first GHCR package publication may be private by default; change the package visibility to Public in the package settings if anonymous pulls are desired.

For implementation and maintenance rules, read [AGENTS.md](AGENTS.md). 
