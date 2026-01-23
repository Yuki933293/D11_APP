#!/bin/sh

# === 配置区域 ===
WIFI_SSID="Luxshare-Guest"
WIFI_PSK="NJ@luxshare"
# ================

echo "正在配置 Wi-Fi: $WIFI_SSID ..."

# 1. 生成 wpa_supplicant 配置文件
# 注意：我们将配置文件放在 /userdata 下，因为这里通常是可读写的
CONFIG_FILE="/userdata/wpa_supplicant.conf"

cat > $CONFIG_FILE <<EOF
ctrl_interface=/var/run/wpa_supplicant
ap_scan=1
update_config=1

network={
    ssid="$WIFI_SSID"
    psk="$WIFI_PSK"
    key_mgmt=WPA-PSK
    priority=1
}
EOF

echo "配置文件已生成: $CONFIG_FILE"

# 2. 杀掉旧的进程 (防止冲突)
killall wpa_supplicant 2>/dev/null
killall udhcpc 2>/dev/null

# 3. 启动 wpa_supplicant
# -B: 后台运行
# -i: 指定网卡接口
# -c: 指定配置文件
echo "启动 wpa_supplicant..."
wpa_supplicant -B -i wlan0 -c $CONFIG_FILE

# 4. 等待几秒让它连接
echo "等待连接 (5秒)..."
sleep 5

# 5. 获取 IP 地址 (DHCP)
echo "正在获取 IP 地址..."
udhcpc -i wlan0 -q

# 6. 验证结果
IP_ADDR=$(ifconfig wlan0 | grep "inet " | awk '{print $2}' | cut -d: -f2)

if [ -n "$IP_ADDR" ]; then
    echo "✅ 配网成功！IP 地址: $IP_ADDR"
    ping -c 3 www.baidu.com
else
    echo "❌ 获取 IP 失败，请检查密码或 Wi-Fi 信号。"
fi
