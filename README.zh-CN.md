# plex-strm-proxy

[English](README.md) · 简体中文

帮助 Plex 播放 `.strm` 文件所引用媒体的 Go 反向代理，面向 Plex Web、app.plex.tv 和官方 Plex 客户端。

代理不会修改 Plex 的媒体库和数据库。

## 工作方式

对于 STRM 媒体，代理首先尝试让客户端直接获取 `.strm` 中的真实 HTTP(S) 媒体地址。这是最节省资源的路径，因为媒体字节不会经过 Plex 或 NAS。

代理会向 Plex 提供选中媒体的信息，并让 Plex 按原生流程做播放决策。如果 Plex 选择 Direct Play，代理会为选中的 Part 提供经过校验的真实媒体地址；如果 Plex 选择 Direct Stream 或转码，则继续由 Plex 负责。普通 Plex 媒体保持透明转发。

只有在 Plex 以 HTTP 错误拒绝原生 STRM 决策或启动请求，且启用了可选兜底时，代理才可能使用 FFmpeg。兜底会尽量优先 Copy，但可能占用代理 CPU、临时磁盘空间和网络带宽。关闭兜底时，请求会保持在 Plex 原生流程中。

## 前置条件

- Plex Media Server 中已经能看到 `.strm` 媒体条目。
- 容器需要以只读方式访问保存 `.strm` 文件的目录。
- 使用 redirect 模式时，播放客户端能够访问真实媒体 URL。
- 媒体源最好支持 HTTP Range 请求。Range 请求用于拖动进度和分段读取。
- 只有启用 STRM HLS 兜底时才需要 FFmpeg。

## 快速开始

已发布的镜像位于 GHCR。镜像内包含用于 STRM HLS 兼容兜底的 FFmpeg；使用 redirect 直连模式时不会调用 FFmpeg。

```bash
docker pull ghcr.io/stromkuo/plex-strm-proxy:latest
```

如果希望固定使用某个版本，可以将 `latest` 换成类似 `1.0.0` 的版本标签。

```bash
docker run --rm \
  --name plex-strm-proxy \
  -p 3001:3001 \
  -e PLEX_UPSTREAM=http://plex:32400 \
  -e STRM_ROOT=/media \
  -v /path/to/plex/media:/media:ro \
  ghcr.io/stromkuo/plex-strm-proxy:latest
```

如果不使用已发布镜像，也可以在本地构建：

```bash
docker build -t plex-strm-proxy .
```

挂载配置中，左侧是宿主机路径，右侧是容器内路径。代理容器内的路径必须和 Plex 使用的 `.strm` 路径以及 `STRM_ROOT` 配置一致。

原生 STRM 播放要求 Plex 能访问代理提供 metadata/Part 的回调地址。默认情况下，回调地址由 `LISTEN_ADDR` 推导；只有 Plex 与代理共享网络命名空间，或确实能访问该地址时才适用。使用 bridge 网络或宿主机与容器端口不一致时，应将 `PLEX_CALLBACK_URL` 设置为 Plex 实际能访问的 HTTP(S) 地址。这个配置只用于 Plex 到代理的请求；客户端、FFprobe 和 FFmpeg 仍使用代理的正常监听地址。

例如，Plex 容器使用：

```bash
-v /host/media:/media
```

那么代理容器应使用：

```bash
-v /host/media:/media:ro
-e STRM_ROOT=/media
```

如果有多个 STRM 根目录，可以用 `STRM_ROOTS` 替代 `STRM_ROOT`：

```bash
-e STRM_ROOTS=/Movies,/TV
-v /path/to/movies:/Movies:ro
-v /path/to/tv:/TV:ro
```

## Plex 配置

在 Plex Server Settings → Network → `Custom server access URLs` 中填入客户端可以访问的代理地址。

示例：

```text
http://plex-proxy.example.com:3001
https://plex-proxy.example.com
```

如果通过公网访问，应使用带有效证书的 HTTPS 地址。HTTPS 可以由外部反向代理或 Tunnel 提供。

修改地址后，如果客户端仍使用旧的服务器连接，可以退出账号后重新登录。测试时应同时播放普通 Plex 媒体和 STRM 媒体。

### 连接路径选择

在局域网内，Plex 直连通常使用 Plex 服务器局域网地址的 `32400` 端口；本代理监听 `3001` 端口。如果客户端直接连接 Plex 服务器的 `32400` 端口，就会绕过代理。要在局域网内测试或使用代理，应让客户端访问 Plex 服务器局域网地址的 `3001` 端口，或访问解析到代理的局域网 DNS 名称。

如果要在公网环境下通过官方 App 或 app.plex.tv 使用本项目，应配置一个公网可访问的自定义服务器访问 URL，实际使用建议是带有效证书的 HTTPS 域名。对于本项目的公网代理部署，应关闭 Plex 的 `Remote Access`，避免 Plex 同时发布自己的公网 `32400` 直连入口。

## 常用配置

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `PLEX_UPSTREAM` | `http://plex:32400` | Plex Media Server 地址 |
| `LISTEN_ADDR` | `0.0.0.0:3001` | 代理监听地址 |
| `PLEX_CALLBACK_URL` | 由 `LISTEN_ADDR` 推导 | 原生 STRM 播放期间 Plex 获取代理本地 metadata/Part 所使用的地址 |
| `STRM_ROOT` / `STRM_ROOTS` | `/media` | 只读 STRM 根目录 |
| `PLAYBACK_MODE` | `redirect` | `redirect` 重定向到媒体源；`proxy` 让媒体流量经过本服务 |
| `STRM_HLS_TRANSCODE` | `true` | 启用 STRM HLS 兼容兜底 |

出于安全考虑，默认允许标准端口上的 HTTP/HTTPS 媒体 URL。私网目标和非标准端口默认禁止，必须显式启用。

### 局域网自建媒体源

一种常见做法是让 `.strm` 指向局域网内的自建服务，例如运行在同一台 NAS 上的 OpenList/alist。这时有两个配置项需要关注：

- `ALLOW_PRIVATE_TARGETS=true` —— 代理默认拒绝私网媒体目标（SSRF 防护）。只有当 STRM URL 指向你自己的局域网地址时才应开启。
- `--add-host media.example.com:192.168.1.10` —— 当 STRM 域名在容器内外解析结果不一致时需要。典型场景：域名走公网 CDN 代理（如 Cloudflare），而服务实际部署在局域网。容器使用路由器 DNS，它自身的出站流量（FFprobe 探测，以及 `proxy` 模式下的全部媒体流量）会解析到公网地址、绕行公网边缘节点。该参数把域名固定到局域网地址；由于固定后的地址是私网 IP，因此必须同时开启 `ALLOW_PRIVATE_TARGETS=true`。

| STRM URL 指向 | 配置 |
| --- | --- |
| 公网地址（网盘链接、公网 alist） | 默认配置，无需额外设置 |
| 局域网服务，域名对所有客户端都解析到局域网 IP | 只需 `ALLOW_PRIVATE_TARGETS=true` |
| 局域网服务，域名走公网 CDN 代理 | `--add-host` 和 `ALLOW_PRIVATE_TARGETS=true` 都要加 |

```bash
docker run ... \
  -e ALLOW_PRIVATE_TARGETS=true \
  --add-host oplist.example.com:192.168.1.10 \
  ghcr.io/stromkuo/plex-strm-proxy:latest
```

`redirect` 模式下客户端仍会自行解析媒体 URL，因此每台客户端也需要能正确解析该域名。如果部分客户端解析效果差，可以在路由器上添加本地 DNS 记录，或改用 `proxy` 模式 —— 媒体流量经过代理容器，只有代理自身的解析会影响播放。

## 限制

- redirect 模式要求播放客户端自身能够访问外部媒体 URL。
- 如果 Plex 原生 STRM 决策或 start 请求被拒绝，可以启用兼容兜底（`STRM_HLS_TRANSCODE`）处理该 STRM 会话。关闭时请求保持在 Plex 原生流程中，代理不会启动 FFmpeg 进程。
- 如果媒体链接带有签名或过期时间参数，该链接必须在整个播放期间保持有效，并且媒体源需要支持分段读取和拖动进度（HTTP Range）。
- HLS 兜底会占用代理 CPU、临时磁盘空间和网络带宽。
- HLS 长距离拖动可能临时占用额外的磁盘空间和网络带宽。
- 是否能播放仍取决于客户端对视频编码、容器、音频格式和字幕的支持。

## 排障

复现问题时，可以跟踪代理日志：

```bash
docker logs -f plex-strm-proxy
```

请依次确认：

1. 客户端可以访问 `Custom server access URLs` 中配置的地址；
2. 客户端可以访问 STRM 文件中的真实媒体 URL；
3. 媒体源支持 Range 请求；
4. Plex Server 和代理之间可以互相访问；
5. 普通 Plex 媒体仍能播放，以区分 Plex 网络问题和 STRM 兼容性问题。

## 开发

```bash
go test ./...
```

GitHub Actions 会在 push 和 Pull Request 时运行测试并验证 Docker 构建。发布带有 `v1.0.0` 这类标签的 GitHub Release 后，Action 会构建多架构镜像并发布到 GHCR：

```text
ghcr.io/stromkuo/plex-strm-proxy:1.0.0
ghcr.io/stromkuo/plex-strm-proxy:latest
```

只有非预发布版本会更新 `latest` 标签。GHCR 第一次创建镜像包时可能默认为 Private；如果希望用户无需登录即可拉取镜像，需要在镜像包设置中将可见性改为 Public。

实现和维护规则请阅读 [AGENTS.md](AGENTS.md)。
