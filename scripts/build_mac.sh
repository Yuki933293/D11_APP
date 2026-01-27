#!/bin/sh
set -eu

# macOS 构建 Python 版本（需提前安装 pyinstaller）
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT_DIR"

if ! command -v pyinstaller >/dev/null 2>&1; then
  echo "未找到 pyinstaller，请先在 macOS 安装（pip install pyinstaller）"
  exit 1
fi

rm -rf "$ROOT_DIR/dist" "$ROOT_DIR/build"
pyinstaller --clean --noconfirm ai_box_py.spec

echo "构建完成：$ROOT_DIR/dist/ai_box_py"
