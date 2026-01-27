#!/bin/sh
set -eu

# 使用 Docker 构建 WebRTC VAD 动态库（Linux/ARM64）
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

docker run --rm --platform linux/arm64 \
  -v "$PWD":/app -w /app/libs/go-webrtc-vad \
  debian:bookworm-slim bash -lc \
  "apt-get update && apt-get install -y build-essential && \
   gcc -O2 -fPIC -shared -I. -o /app/libwebrtcvad.so webrtc.c"

echo "构建完成：$ROOT_DIR/libwebrtcvad.so"
