# Plex 原生决策脱敏基线

这些 XML 是从 Plex Web 播放本地媒体和 STRM 媒体时观察到的响应形状中脱敏整理的回放样本。样本不包含真实媒体路径、签名 URL、IP、token 或 Plex 账号信息。

| Fixture | 媒体类型 | 客户端意图 | Plex 原生结果 | 代理应做的事 |
| --- | --- | --- | --- | --- |
| `local-h264-eac3.xml` | 本地 MKV，H.264 视频，E-AC-3 5.1 音频 | DASH，`directPlay=0`，`directStream=1` | Part 为 `transcode`，视频 copy，音频转码 | 保持 Plex 响应和 Plex 转码归属 |
| `local-hevc-truehd.xml` | 本地 MKV，HEVC 视频，TrueHD 7.1 音频 | DASH，`directPlay=0`，`directStream=0` | Plex 返回 H.264/AAC DASH，Part 和选中流均为 `transcode` | 保持 Plex 响应和 Plex 转码归属 |
| `strm-hevc-eac3-aac-direct.xml` | STRM 指向 MKV，HEVC 视频，E-AC-3 与 AAC 音频 | 首次 DASH，`directPlay=0`，`directStream=1` | Part 为 `directplay`，选中的流为 copy | 只把代理 Part 的 `file` 替换为已校验的源地址 |
| `strm-hevc-native-stream-transcode.xml` | 同类 STRM，HEVC 视频，客户端不能直接使用该组合 | DASH，`directPlay=0`，`directStream=0` | Part 仍可能是 `directplay`，但选中的视频/音频流均为 `transcode` | 视为 Plex 转码，不得重写为 302，也不得记入 direct 缓存 |
| `strm-hevc-truehd-native-copy.xml` | STRM 指向 MKV，HEVC 视频，TrueHD 音频 | Plex Web DASH，`directPlay=0`，`directStream=1`，随后由 Plex 打开 start.mpd | Part 为 `directplay`，视频为 `copy`，选中音频为 `transcode` | 仍归 Plex；允许 Plex 生成“视频 copy + 音频 AAC”输出，不创建代理 HLS |

判断依据是选中的 Part 和 Stream 的决策字段；设备型号、媒体标题和 User-Agent 不参与判断。
