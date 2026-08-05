#!/bin/bash
# P5 打包脚本：Windows 绿色版（免安装目录 → zip）
set -e
cd /home/alan/AC_WorkSpace/edison_ws/DevSpace/rtsp-streaming-service-2026-08-04

echo "=== [1/5] 交叉编译 Windows 引擎 ==="
cd engine
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" -o bin/rtsp-engine.exe ./cmd/rtsp-engine
ls -lh bin/rtsp-engine.exe
cd ..

echo "=== [2/5] 下载 Windows FFmpeg ==="
if [ ! -f engine/bin/ffmpeg.exe ]; then
  curl -sL -o /tmp/ffmpeg-win.zip "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip" --connect-timeout 20 --max-time 300
  ls -lh /tmp/ffmpeg-win.zip
  rm -rf /tmp/ffmpeg-win && mkdir -p /tmp/ffmpeg-win && cd /tmp/ffmpeg-win
  unzip -q /tmp/ffmpeg-win.zip
  FFDIR=$(ls -d ffmpeg-*/)
  cp "$FFDIR/bin/ffmpeg.exe" "$FFDIR/bin/ffprobe.exe" /home/alan/AC_WorkSpace/edison_ws/DevSpace/rtsp-streaming-service-2026-08-04/engine/bin/
fi
ls -lh /home/alan/AC_WorkSpace/edison_ws/DevSpace/rtsp-streaming-service-2026-08-04/engine/bin/

echo "=== [3/5] electron-builder 生成 win-unpacked ==="
cd /home/alan/AC_WorkSpace/edison_ws/DevSpace/rtsp-streaming-service-2026-08-04/ui
export ELECTRON_MIRROR="https://npmmirror.com/mirrors/electron/"
npx electron-builder --win dir --config.win.signAndEditExecutable=false 2>&1 | tail -15
cd /home/alan/AC_WorkSpace/edison_ws/DevSpace/rtsp-streaming-service-2026-08-04

echo "=== [4/5] 压缩发布包 ==="
VER=$(grep '"version"' ui/package.json | head -1 | sed 's/[^0-9.]//g')
mkdir -p dist
cd ui/dist
zip -qr "../../dist/RTSP-Streaming-Service-win64-v${VER}.zip" win-unpacked
cd /home/alan/AC_WorkSpace/edison_ws/DevSpace/rtsp-streaming-service-2026-08-04

echo "=== [5/5] 完成 ==="
ls -lh "dist/RTSP-Streaming-Service-win64-v${VER}.zip"
