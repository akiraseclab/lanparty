#!/usr/bin/env bash
#
# install-server.sh —— 把 lanparty 协调节点(coord) 一键安装为 systemd 服务
#
# 用法：
#   sudo ./install-server.sh <lanparty二进制路径> <网络名> <PSK> [监听地址，默认 0.0.0.0:7800]
#
# 示例：
#   sudo ./install-server.sh ./lanparty party-lan "$(openssl rand -hex 16)"
#   sudo ./install-server.sh ./lanparty party-lan 'my-psk' 0.0.0.0:7800
#
# 行为：
#   1. 安装二进制到 /usr/local/bin/lanparty
#   2. 用 heredoc 生成 /etc/systemd/system/lanparty-coord.service
#      （Restart=always / RestartSec=5 / After=network.target / WantedBy=multi-user.target）
#   3. systemctl daemon-reload && systemctl enable --now lanparty-coord
#   4. systemctl status --no-pager 显示结果，并提示放行 TCP 端口
#
# 说明：协调节点(coord) 只做 TCP 汇聚/转发，普通用户即可运行，不需要 root 特权。
#
set -euo pipefail

UNIT=/etc/systemd/system/lanparty-coord.service
INSTALL_PATH=/usr/local/bin/lanparty
SERVICE=lanparty-coord.service

usage() {
  echo "用法: sudo $0 <lanparty二进制路径> <网络名> <PSK> [监听地址，默认 0.0.0.0:7800]"
}

if [[ $# -lt 3 || $# -gt 4 ]]; then
  usage
  exit 1
fi

BIN_SRC=$1
NETWORK=$2
PSK=$3
LISTEN=${4:-0.0.0.0:7800}

if [[ ${EUID} -ne 0 ]]; then
  echo "错误：请用 sudo/root 运行本脚本。" >&2
  exit 1
fi

if [[ ! -f "$BIN_SRC" || ! -x "$BIN_SRC" ]]; then
  echo "错误：二进制不存在或不可执行：$BIN_SRC" >&2
  usage
  exit 1
fi

# unit 文件的 ExecStart 用双引号包裹参数；这里拒绝会破坏 unit 文件解析的字符
# （双引号、反斜杠、%、$、换行）。PSK 建议直接用 `openssl rand -hex 16` 生成。
for v in "$NETWORK" "$PSK" "$LISTEN"; do
  if [[ "$v" == *$'\n'* ]] || grep -qE '[%"\\$]' <<<"$v"; then
    echo "错误：参数含有 systemd 特殊字符（双引号/反斜杠/%/\$/换行），请更换后重试。" >&2
    exit 1
  fi
done

PORT=${LISTEN##*:}
if [[ ! "$PORT" =~ ^[0-9]+$ ]] || (( PORT < 1 || PORT > 65535 )); then
  echo "错误：监听地址格式应为 地址:端口，例如 0.0.0.0:7800（当前：$LISTEN）" >&2
  exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
  echo "错误：未找到 systemctl，本脚本仅支持 systemd 发行版（Ubuntu/Debian 等）。" >&2
  exit 1
fi

echo "==> 停止旧服务（如有，便于覆盖升级二进制）"
systemctl stop "$SERVICE" >/dev/null 2>&1 || true

echo "==> 安装二进制：$BIN_SRC -> $INSTALL_PATH"
install -m 0755 "$BIN_SRC" "$INSTALL_PATH"

echo "==> 准备运行账号 lanparty（coord 不需要 root，用专用账号更安全）"
RUN_USER_LINE=""
if id lanparty >/dev/null 2>&1; then
  RUN_USER_LINE="User=lanparty"
elif useradd --system --no-create-home --shell /usr/sbin/nologin lanparty 2>/dev/null; then
  RUN_USER_LINE="User=lanparty"
else
  echo "警告：创建系统用户 lanparty 失败，服务将以 root 运行（coord 本不需要特权）。"
fi

echo "==> 生成 unit 文件：$UNIT"
cat > "$UNIT" <<EOF
[Unit]
Description=lanparty coordination node (network: ${NETWORK})
After=network.target

[Service]
Type=simple
# coord 只做 TCP 汇聚/转发，无需任何特权，使用专用系统账号运行
${RUN_USER_LINE}
ExecStart=${INSTALL_PATH} coord --network "${NETWORK}" --psk "${PSK}" --listen "${LISTEN}"
# 进程异常退出后 5 秒自动拉起
Restart=always
RestartSec=5
# 轻量加固：coord 不写文件、不加载内核模块，可以安全收紧
NoNewPrivileges=yes
ProtectSystem=full
ProtectHome=yes
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF

echo "==> systemctl daemon-reload && enable --now"
systemctl daemon-reload
systemctl enable --now "$SERVICE"

# 等服务进入 active（最多 5 秒），避免 status 抢跑
for _ in $(seq 1 10); do
  if [[ "$(systemctl is-active "$SERVICE" 2>/dev/null || true)" == "active" ]]; then
    break
  fi
  sleep 0.5
done

echo "==> 服务状态"
systemctl status --no-pager "$SERVICE" || true

cat <<EOF

安装完成！
  服务名   : ${SERVICE}
  监听     : ${LISTEN}
  二进制   : ${INSTALL_PATH}
  实时日志 : journalctl -u lanparty-coord -f

请放行 TCP ${PORT} 端口（成员节点必须能连到 coord）：
  Ubuntu/Debian (ufw)  : sudo ufw allow ${PORT}/tcp
  firewalld            : sudo firewall-cmd --permanent --add-port=${PORT}/tcp && sudo firewall-cmd --reload
  iptables             : sudo iptables -I INPUT -p tcp --dport ${PORT} -j ACCEPT
  云服务器              : 另需在控制台安全组放行 TCP ${PORT}

EOF
