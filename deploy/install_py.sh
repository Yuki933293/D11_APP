#!/bin/sh
set -eu

# =========================
# RK3308 AI_BOX Python 版本部署脚本
# =========================

log() { echo "[install-py] $*"; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="${1:-$SCRIPT_DIR/ai_box.env}"
OS_NAME="$(uname -s 2>/dev/null || echo unknown)"

generate_env() {
  cat >"$ENV_FILE" <<'EOF'
# =========================
# RK3308 AI_BOX Python 版本配置
# =========================
#
# 用法：
# 1) 填写真实参数（不要提交包含 Key/密码的文件到 git）
# 2) 板子上运行：sh deploy/install_py.sh /path/to/ai_box.env
# 3) 或者把 env 放到：/userdata/AI_BOX/ai_box.env，程序会自动读取（也可用 AI_BOX_ENV_FILE 指定路径）

# -------------------------
# 必填：云端 Key（DashScope）
# -------------------------
AI_BOX_DASH_API_KEY=

# -------------------------
# 伪唤醒（云端 ASR + 本地门控）
# -------------------------
# 唤醒词（逗号分隔，支持中文逗号）
AI_BOX_WAKE_WORDS=你好小瑞,小瑞,小睿,晓瑞
# 唤醒态空闲多久回休眠（支持 90s / 2m / 1500ms）
AI_BOX_WAKE_IDLE_TIMEOUT=90s
# 仅“纯唤醒词”时播放的确认文本
AI_BOX_WAKE_ACK_TEXT=我在

# -------------------------
# 模型配置（可选）
# -------------------------
AI_BOX_LLM_MODEL_FAST=qwen-turbo-latest
AI_BOX_LLM_MODEL_SEARCH=qwen-max

AI_BOX_ASR_MODEL=paraformer-realtime-v2
AI_BOX_ASR_SAMPLE_RATE=16000

AI_BOX_TTS_MODEL=cosyvoice-v3-plus
AI_BOX_TTS_VOICE=longanhuan
AI_BOX_TTS_SAMPLE_RATE=22050
AI_BOX_TTS_VOLUME=50

# -------------------------
# VAD（仅支持 WebRTC VAD）
# -------------------------
# 取值 0~3（越大越严格）
AI_BOX_VAD_MODE=3

# -------------------------
# 路径（可选）
# -------------------------
AI_BOX_HOME=/userdata/AI_BOX
AI_BOX_MUSIC_DIR=/userdata/AI_BOX/music

# -------------------------
# 录音参数（可选，默认适配 RK3308 10 麦阵列）
# -------------------------
AI_BOX_ARECORD_DEVICE=hw:2,0
AI_BOX_ARECORD_CHANNELS=10
AI_BOX_ARECORD_RATE=16000
AI_BOX_ARECORD_PERIOD_SIZE=256
AI_BOX_ARECORD_BUFFER_SIZE=16384

# -------------------------
# WiFi（install_py.sh 使用；ai_box 本体不会读取）
# -------------------------
WIFI_ENABLE=0
WIFI_AUTOSTART=0
WIFI_IFACE=wlan0
WIFI_SSID=
WIFI_PSK=
WIFI_COUNTRY=CN
WIFI_CONF_PATH=/userdata/cfg/wpa_supplicant.conf

# -------------------------
# 时区（install_py.sh 可选设置）
# -------------------------
TZ=Asia/Shanghai
EOF
}

if [ "$OS_NAME" != "Linux" ] || [ ! -d /userdata ]; then
  if [ ! -f "$ENV_FILE" ]; then
    log "未找到配置文件：$ENV_FILE"
    log "生成默认 ai_box.env（仅生成，不执行安装）"
    generate_env
  fi
  log "检测到非 Linux 环境或无 /userdata，仅生成/更新 env 文件，跳过安装"
  exit 0
fi

if [ ! -f "$ENV_FILE" ]; then
  log "未找到配置文件：$ENV_FILE"
  log "生成默认 ai_box.env，请填写 AI_BOX_DASH_API_KEY 后重新运行"
  generate_env
  exit 1
fi

# shellcheck disable=SC1090
set -a
. "$ENV_FILE"
set +a

AI_BOX_HOME="${AI_BOX_HOME:-/userdata/AI_BOX}"
AI_BOX_MUSIC_DIR="${AI_BOX_MUSIC_DIR:-$AI_BOX_HOME/music}"
ENV_TARGET="$AI_BOX_HOME/ai_box.env"

abs_path() {
  p="$1"
  if command -v readlink >/dev/null 2>&1; then
    rp="$(readlink -f "$p" 2>/dev/null || true)"
    if [ -n "$rp" ]; then
      echo "$rp"
      return 0
    fi
  fi
  dir="$(dirname "$p")"
  base="$(basename "$p")"
  (cd "$dir" 2>/dev/null && printf "%s/%s\n" "$(pwd -P)" "$base") || echo "$p"
}

find_py_bin() {
  if [ -n "${AI_BOX_PY_SRC:-}" ] && [ -f "$AI_BOX_PY_SRC" ]; then
    echo "$AI_BOX_PY_SRC"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/../dist/ai_box_py" ]; then
    echo "$SCRIPT_DIR/../dist/ai_box_py"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/ai_box_py" ]; then
    echo "$SCRIPT_DIR/ai_box_py"
    return 0
  fi
  return 1
}

find_luxso() {
  if [ -n "${AI_BOX_LUXSO_SRC:-}" ] && [ -f "$AI_BOX_LUXSO_SRC" ]; then
    echo "$AI_BOX_LUXSO_SRC"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/../libluxaudio.so" ]; then
    echo "$SCRIPT_DIR/../libluxaudio.so"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/libluxaudio.so" ]; then
    echo "$SCRIPT_DIR/libluxaudio.so"
    return 0
  fi
  return 1
}

find_webrtc_so() {
  if [ -n "${AI_BOX_WEBRTCVAD_SRC:-}" ] && [ -f "$AI_BOX_WEBRTCVAD_SRC" ]; then
    echo "$AI_BOX_WEBRTCVAD_SRC"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/../libwebrtcvad.so" ]; then
    echo "$SCRIPT_DIR/../libwebrtcvad.so"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/libwebrtcvad.so" ]; then
    echo "$SCRIPT_DIR/libwebrtcvad.so"
    return 0
  fi
  return 1
}

BIN_SRC="$(find_py_bin || true)"
if [ -z "$BIN_SRC" ]; then
  log "找不到 ai_box_py 二进制文件（可用 AI_BOX_PY_SRC 指定路径）"
  exit 1
fi

mkdir -p "$AI_BOX_HOME" "$AI_BOX_MUSIC_DIR"

log "安装目录：$AI_BOX_HOME"
BIN_DST="$AI_BOX_HOME/ai_box_py"
if [ "$(abs_path "$BIN_SRC")" = "$(abs_path "$BIN_DST")" ]; then
  log "ai_box_py 已在目标目录，跳过复制：$BIN_DST"
else
  cp "$BIN_SRC" "$BIN_DST"
fi
chmod +x "$AI_BOX_HOME/ai_box_py" || true

LUXSO_SRC="$(find_luxso || true)"
if [ -n "$LUXSO_SRC" ]; then
  SO_DST="$AI_BOX_HOME/libluxaudio.so"
  if [ "$(abs_path "$LUXSO_SRC")" = "$(abs_path "$SO_DST")" ]; then
    log "libluxaudio.so 已在目标目录，跳过复制：$SO_DST"
  else
    cp "$LUXSO_SRC" "$SO_DST"
  fi
fi

WEBRTC_SRC="$(find_webrtc_so || true)"
if [ -n "$WEBRTC_SRC" ]; then
  VAD_DST="$AI_BOX_HOME/libwebrtcvad.so"
  if [ "$(abs_path "$WEBRTC_SRC")" = "$(abs_path "$VAD_DST")" ]; then
    log "libwebrtcvad.so 已在目标目录，跳过复制：$VAD_DST"
  else
    cp "$WEBRTC_SRC" "$VAD_DST"
  fi
fi

if [ "$(abs_path "$ENV_FILE")" = "$(abs_path "$ENV_TARGET")" ]; then
  log "配置文件已在目标位置，跳过复制：$ENV_TARGET"
else
  cp "$ENV_FILE" "$ENV_TARGET"
fi

cat >"$AI_BOX_HOME/run_py.sh" <<'EOF'
#!/bin/sh
set -eu
AI_BOX_HOME="${AI_BOX_HOME:-/userdata/AI_BOX}"
ENV_FILE="${AI_BOX_ENV_FILE:-$AI_BOX_HOME/ai_box.env}"

if [ -f "$ENV_FILE" ]; then
  # shellcheck disable=SC1090
  set -a
  . "$ENV_FILE"
  set +a
fi

export LD_LIBRARY_PATH="$AI_BOX_HOME${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
export AI_BOX_ENV_FILE="$ENV_FILE"

cd "$AI_BOX_HOME"
exec ./ai_box_py
EOF
chmod +x "$AI_BOX_HOME/run_py.sh" || true

# 可选：设置时区（不一定所有系统都有 /etc/timezone）
if [ -n "${TZ:-}" ] && [ -d /etc ]; then
  ln -snf "/usr/share/zoneinfo/$TZ" /etc/localtime 2>/dev/null || true
  echo "$TZ" >/etc/timezone 2>/dev/null || true
fi

wifi_write_conf() {
  WIFI_CONF_PATH="${WIFI_CONF_PATH:-/userdata/cfg/wpa_supplicant.conf}"
  WIFI_COUNTRY="${WIFI_COUNTRY:-CN}"
  mkdir -p "$(dirname "$WIFI_CONF_PATH")"
  cat >"$WIFI_CONF_PATH" <<EOF
ctrl_interface=/var/run/wpa_supplicant
update_config=1
country=$WIFI_COUNTRY

network={
  ssid="$WIFI_SSID"
  psk="$WIFI_PSK"
  key_mgmt=WPA-PSK
  scan_ssid=1
}
EOF
  log "已写入 WiFi 配置：$WIFI_CONF_PATH"
}

wifi_up() {
  WIFI_IFACE="${WIFI_IFACE:-wlan0}"
  WIFI_CONF_PATH="${WIFI_CONF_PATH:-/userdata/cfg/wpa_supplicant.conf}"

  if command -v ip >/dev/null 2>&1; then
    ip link set "$WIFI_IFACE" up 2>/dev/null || true
  elif command -v ifconfig >/dev/null 2>&1; then
    ifconfig "$WIFI_IFACE" up 2>/dev/null || true
  fi

  if command -v wpa_supplicant >/dev/null 2>&1; then
    killall wpa_supplicant 2>/dev/null || true
    wpa_supplicant -B -i "$WIFI_IFACE" -c "$WIFI_CONF_PATH" 2>/dev/null || true
  fi

  if command -v udhcpc >/dev/null 2>&1; then
    killall udhcpc 2>/dev/null || true
    udhcpc -i "$WIFI_IFACE" -q -t 5 -n 2>/dev/null || true
  fi
}

if [ "${WIFI_ENABLE:-0}" = "1" ]; then
  if [ -n "${WIFI_SSID:-}" ] && [ -n "${WIFI_PSK:-}" ]; then
    wifi_write_conf
    wifi_up
  else
    log "WIFI_ENABLE=1 但未配置 WIFI_SSID/WIFI_PSK，跳过 WiFi 配置"
  fi
fi

if [ "${WIFI_AUTOSTART:-0}" = "1" ]; then
  STARTUP_PATH="${STARTUP_PATH:-/userdata/AI_BOX/start_ai_box_py.sh}"
  cat >"$STARTUP_PATH" <<EOF
#!/bin/sh
cd "$AI_BOX_HOME"
exec ./run_py.sh
EOF
  chmod +x "$STARTUP_PATH"
  log "已写入自启动脚本：$STARTUP_PATH"
fi

log "Python 版本安装完成"
