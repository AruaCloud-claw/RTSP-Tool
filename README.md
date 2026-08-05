# RTSP Tool

RTSP 推拉流服务（Windows 绿色版）。基于 **Go 引擎 + Electron 界面** 架构，提供视频流的推流、拉流、转发与低延迟预览能力。

## 核心能力

- **三种输入源**：视频文件 / USB 摄像头（Windows dshow）/ 外部 RTSP 拉流
- **内置 RTSP 服务**：统一对外提供 `rtsp://host:8554/live/xxx` 拉流地址
- **第三方推流接入**：支持任意 RTSP 推流端（如 ffmpeg）推送到本机并统一显示
- **WebRTC 低延迟预览**：零转码 RTP 直通，端到端延迟约 50~150ms（局域网）
- **辅助功能**：快照抓帧、实时监控（WebSocket）、RTSP Basic 鉴权、流配置持久化、端口冲突自动顺延

## 架构

```
┌─────────────┐      ┌──────────────────────────────────────────┐
│  Electron   │ REST │                 Go 引擎                    │
│  UI (Vue3)  │◄────►│  ┌────────┐   ┌────────┐   ┌──────────┐  │
│  推流/拉流/  │  WS  │  │ 管道管理 │──►│ RTSP服务 │──► 拉流客户端 │  │
│  服务/设置   │      │  │(源→RTP) │   │ :8554  │   │  (VLC等)  │  │
└─────────────┘      │  └───┬────┘   └────────┘   └──────────┘  │
                     │      │ 同一份 RTP 包                      │
                     │      ▼                                   │
                     │  ┌──────────┐   ┌─────────────────────┐  │
                     │  │ WebRTC   │──►│ 浏览器低延迟预览      │  │
                     │  │ (pion)   │   │ (零转码直通)          │  │
                     │  └──────────┘   └─────────────────────┘  │
                     └──────────────────────────────────────────┘
```

**关键设计**：媒体引擎为独立 Go 进程，Electron 仅做 UI 壳；推流、拉流、WebRTC 预览共用同一份 RTP 数据（零转码），保证低延迟与低资源消耗。

## 技术栈

| 层 | 技术 |
|---|---|
| 引擎 | Go 1.26 + gortsplib（RTSP）+ pion/webrtc（WebRTC）+ FFmpeg 子进程 |
| 界面 | Electron + 原生 HTML/JS（Vue3 风格） |
| 平台 | Windows 10/11 x64（绿色版，免安装） |

## 目录结构

```
engine/           Go 媒体引擎
  cmd/            rtsp-engine 主程序 + 调试工具
  internal/       config / api / stream / pipeline / rtsp / webrtc / h264 / source / ffmpeg
  third_party/    vendored gortsplib（goproxy.cn 源损坏，本地干净副本）
ui/               Electron 界面
  main.js        引擎生命周期管理（启动/守护/日志转发）
  renderer/      页面（推流/拉流/服务/设置）
docs/             技术方案文档
```

## 构建

```bash
# 引擎（Windows 交叉编译）
cd engine
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o bin/rtsp-engine.exe ./cmd/rtsp-engine

# UI（需要 Electron 依赖）
cd ui
npm install
npx electron-builder --win dir
```

> 注意：`engine/third_party/gortsplib` 为 vendored 副本（v3.11.0），因 goproxy.cn 的 v3.11.1 压缩包损坏，本地副本干净无调试代码，构建时直接使用。

## 使用

1. 启动软件 → 引擎自动运行（默认 HTTP `127.0.0.1:18080`，RTSP `:8554`，端口被占自动顺延）
2. **推流页**：选文件/摄像头/RTSP 源 → 创建 → 右侧列表管理 → 预览
3. **拉流页**：填外部 RTSP 地址 → 创建 → 右侧多路实时预览
4. **服务页**：第三方推流到 `rtsp://本机IP:8554/live/xxx` → 列表显示 → 预览
5. 其他播放器拉流：`rtsp://127.0.0.1:8554/live/<路径>`

## License

内部项目，版权所有。
