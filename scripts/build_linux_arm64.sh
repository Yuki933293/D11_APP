#!/bin/sh
set -eu

# 使用 Docker 构建 Linux/ARM64 (glibc) 可执行文件
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

rm -rf "$ROOT_DIR/dist" "$ROOT_DIR/build"
docker run --rm --platform linux/arm64 \
  -v "$PWD":/app -w /app \
  python:3.11-slim-bookworm bash -lc \
  "apt-get update && apt-get install -y binutils && \
   pip install pyinstaller && \
   pyinstaller --clean --noconfirm ai_box_py.spec"

echo "构建完成：$ROOT_DIR/dist/ai_box_py"
