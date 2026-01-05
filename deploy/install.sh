#!/bin/sh
set -eu

# =========================
# RK3308 AI_BOX 一键部署脚本
# =========================
# 说明：
# - 读取一个 env 配置文件（KEY=VALUE），完成：
#   1) 安装 ai_box 到 /userdata/AI_BOX（可通过 AI_BOX_HOME 覆盖）
#   2) 写入 ai_box.env（供程序自动读取）
#   3) 可选：配置/拉起 WiFi
#   4) 可选：安装自启动（systemd 或 /etc/init.d）

log() { echo "[install] $*"; }

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="${1:-$SCRIPT_DIR/ai_box.env}"

if [ ! -f "$ENV_FILE" ]; then
  log "未找到配置文件：$ENV_FILE"
  log "请复制并填写：deploy/ai_box.env.example -> ai_box.env"
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

find_src() {
  # 允许用户用环境变量指定来源路径
  # AI_BOX_BINARY_SRC=/path/to/ai_box
  # AI_BOX_LUXSO_SRC=/path/to/libluxaudio.so
  if [ -n "${AI_BOX_BINARY_SRC:-}" ] && [ -f "$AI_BOX_BINARY_SRC" ]; then
    echo "$AI_BOX_BINARY_SRC"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/../ai_box" ]; then
    echo "$SCRIPT_DIR/../ai_box"
    return 0
  fi
  if [ -f "$SCRIPT_DIR/ai_box" ]; then
    echo "$SCRIPT_DIR/ai_box"
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

BIN_SRC="$(find_src || true)"
if [ -z "$BIN_SRC" ]; then
  log "找不到 ai_box 二进制文件（可用 AI_BOX_BINARY_SRC 指定路径）"
  exit 1
fi

mkdir -p "$AI_BOX_HOME" "$AI_BOX_MUSIC_DIR"

log "安装目录：$AI_BOX_HOME"
BIN_DST="$AI_BOX_HOME/ai_box"
if [ "$(abs_path "$BIN_SRC")" = "$(abs_path "$BIN_DST")" ]; then
  log "ai_box 已在目标目录，跳过复制：$BIN_DST"
else
  cp "$BIN_SRC" "$BIN_DST"
fi
chmod +x "$AI_BOX_HOME/ai_box" || true

LUXSO_SRC="$(find_luxso || true)"
if [ -n "$LUXSO_SRC" ]; then
  SO_DST="$AI_BOX_HOME/libluxaudio.so"
  if [ "$(abs_path "$LUXSO_SRC")" = "$(abs_path "$SO_DST")" ]; then
    log "libluxaudio.so 已在目标目录，跳过复制：$SO_DST"
  else
    cp "$LUXSO_SRC" "$SO_DST"
  fi
fi

# 写入 env（供程序自动读取）
if [ "$(abs_path "$ENV_FILE")" = "$(abs_path "$ENV_TARGET")" ]; then
  log "配置文件已在目标位置，跳过复制：$ENV_TARGET"
else
  cp "$ENV_FILE" "$ENV_TARGET"
fi

# 运行脚本（自启动/手动启动都用它）
cat >"$AI_BOX_HOME/run.sh" <<'EOF'
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
exec ./ai_box
EOF
chmod +x "$AI_BOX_HOME/run.sh" || true

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
    log "WIFI_ENABLE=1 但未填写 WIFI_SSID/WIFI_PSK，跳过 WiFi 配置"
  fi
fi

install_autostart_initd() {
  INITD_PATH="/etc/init.d/S99ai_box"
  cat >"$INITD_PATH" <<EOF
#!/bin/sh
case "\$1" in
  start)
    echo "[ai_box] start"
    "$AI_BOX_HOME/run.sh" >"$AI_BOX_HOME/ai_box.log" 2>&1 &
    echo \$! >/var/run/ai_box.pid 2>/dev/null || true
    ;;
  stop)
    echo "[ai_box] stop"
    if [ -f /var/run/ai_box.pid ]; then
      kill "\$(cat /var/run/ai_box.pid)" 2>/dev/null || true
      rm -f /var/run/ai_box.pid 2>/dev/null || true
    fi
    killall ai_box 2>/dev/null || true
    killall arecord 2>/dev/null || true
    killall aplay 2>/dev/null || true
    ;;
  restart)
    \$0 stop
    sleep 1
    \$0 start
    ;;
  *)
    echo "Usage: \$0 {start|stop|restart}"
    exit 1
    ;;
esac
exit 0
EOF
  chmod +x "$INITD_PATH" || true
  log "已安装 init.d 自启动：$INITD_PATH"
}

install_autostart_systemd() {
  SERVICE_PATH="/etc/systemd/system/ai_box.service"
  cat >"$SERVICE_PATH" <<EOF
[Unit]
Description=RK3308 AI Box
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=$AI_BOX_HOME
Environment=AI_BOX_ENV_FILE=$AI_BOX_HOME/ai_box.env
Environment=LD_LIBRARY_PATH=$AI_BOX_HOME
ExecStart=$AI_BOX_HOME/run.sh
Restart=always
RestartSec=1

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload || true
  systemctl enable ai_box.service || true
  log "已安装 systemd 自启动：$SERVICE_PATH"
}

if [ "${INSTALL_AUTOSTART:-1}" = "1" ]; then
  if command -v systemctl >/dev/null 2>&1; then
    install_autostart_systemd || true
  elif [ -d /etc/init.d ] && [ -w /etc/init.d ]; then
    install_autostart_initd || true
  else
    log "未检测到可写的 systemd/init.d，自启动未安装（你可以手动执行：$AI_BOX_HOME/run.sh）"
  fi
fi

log "完成。可手动启动：$AI_BOX_HOME/run.sh"
