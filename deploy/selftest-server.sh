#!/usr/bin/env bash
#
# selftest-server.sh —— lanparty Linux 真机端到端自测
#
# 目标：在一台带 /dev/net/tun 的 Linux 服务器上验证 lanparty 虚拟网真正可用：
#   启动 1 个 coord + 2 个 peer（同机双 TUN：lanparty-t0 / lanparty-t1），
#   等虚拟 IP(10.66.0.x) 就绪后，跨 TUN 互 ping（小包 + 1200 字节大包）。
#
# 约定：
#   - 用法: sudo ./selftest-server.sh [lanparty二进制路径，默认 ./lanparty]
#   - 需要 root（peer 创建 TUN、添加路由都需要 root）；
#   - 依赖命令：openssl / ip / ping（Ubuntu/Debian 默认都有）；
#   - 不安装任何持久服务；任何退出路径（成功/失败/Ctrl-C）都会清理
#     全部进程与 TUN 网卡，不在系统上留残留。
#
set -euo pipefail

IFACE0=lanparty-t0          # Linux 网卡名最长 15 字符，这里 11 字符，符合要求
IFACE1=lanparty-t1
BASE_PORT=17800             # 自测 coord 监听端口起点，被占用则 +1 重试
VIP_WAIT_TIMEOUT=20         # 等待虚拟 IP 就绪的超时秒数

# ---------- 全局状态（cleanup 依赖，必须先初始化为空） ----------
BIN=""
WORKDIR=""
LOG_COORD=""; LOG_P0=""; LOG_P1=""
COORD_PID=""; P0_PID=""; P1_PID=""
RP_ALL_SAVE=""; RP_DEFAULT_SAVE=""
PING1="FAIL"; PING2="FAIL"; PING1_OUT=""; PING2_OUT=""
VIP_T0=""; VIP_T1=""; PORT=""
OVERALL_ALL="FAIL"          # 只有两项 ping 全部通过才置为 PASS
START_TS=$SECONDS

log()  { echo "[selftest] $*"; }
warn() { echo "[selftest][警告] $*" >&2; }

# ---------- 统一清理：任何退出路径都会经过这里 ----------
cleanup() {
  trap - EXIT INT TERM          # 防止重入
  local p
  for p in "${COORD_PID:-}" "${P0_PID:-}" "${P1_PID:-}"; do
    if [[ -n "$p" ]]; then
      kill "$p" 2>/dev/null || true
    fi
  done
  sleep 0.5
  for p in "${COORD_PID:-}" "${P0_PID:-}" "${P1_PID:-}"; do
    if [[ -n "$p" ]]; then
      kill -9 "$p" 2>/dev/null || true
    fi
  done
  wait 2>/dev/null || true
  # TUN 网卡：peer 进程死后一般自动消失，这里兜底删除（忽略已不存在的情况）
  ip link del "$IFACE0" 2>/dev/null || true
  ip link del "$IFACE1" 2>/dev/null || true
  restore_rp_filter
  if [[ -n "${WORKDIR:-}" ]]; then
    rm -rf "$WORKDIR"
  fi
  log "清理完成：coord/peer 进程与 ${IFACE0}/${IFACE1} 网卡均已移除，无残留"
}

fail() {
  echo "[selftest][FAIL] $*" >&2
  dump_logs
  dump_state
  finish
  exit 1
}

dump_logs() {
  local item name path
  for item in "coord|$LOG_COORD" "peer-${IFACE0}|$LOG_P0" "peer-${IFACE1}|$LOG_P1"; do
    name=${item%%|*}
    path=${item#*|}
    if [[ -n "$path" && -s "$path" ]]; then
      echo "----- $name 日志（$path，末尾 40 行）-----" >&2
      tail -n 40 "$path" >&2 || true
    fi
  done
}

# 失败时顺带打印网卡/路由现场，便于定位
dump_state() {
  local ifc
  for ifc in "$IFACE0" "$IFACE1"; do
    if ip link show dev "$ifc" >/dev/null 2>&1; then
      echo "----- ip -4 addr show dev $ifc -----" >&2
      ip -4 addr show dev "$ifc" >&2 || true
    fi
  done
  echo "----- ip route（10.66 相关）-----" >&2
  ip route 2>/dev/null | grep -E '10\.66\.0\.' >&2 || true
}

finish() {
  local total=$(( SECONDS - START_TS ))
  echo "=========================================================="
  echo " lanparty 真机自测结果: ${OVERALL_ALL:-FAIL}（耗时 ${total}s）"
  echo "   coord   : 127.0.0.1:${PORT:-?}（进程已清理）"
  echo "   ${IFACE0} VIP : ${VIP_T0:-未获得}"
  echo "   ${IFACE1} VIP : ${VIP_T1:-未获得}"
  echo "   ping 小包 (-c 5 -W 2)    : ${PING1}"
  echo "   ping 大包 (-c 3 -s 1200) : ${PING2}"
  echo "=========================================================="
}

# ---------- rp_filter 临时放宽（关键！）----------
# 同机双 TUN 互 ping 时，回包会从“另一块” TUN 进入本机；若内核 rp_filter
# 为严格模式(1)，回包会被当作伪造包丢弃，造成误报 FAIL。
# 这里把 all/default 临时设为 0（关闭），peer 之后创建的 TUN 也会继承 0；
# 退出时 restore_rp_filter 恢复原值。容器内 /proc/sys 只读时仅警告。
loosen_rp_filter() {
  local base=/proc/sys/net/ipv4/conf
  if [[ -r "$base/all/rp_filter" ]]; then
    RP_ALL_SAVE=$(cat "$base/all/rp_filter" 2>/dev/null || true)
    if ! echo 0 > "$base/all/rp_filter" 2>/dev/null; then
      warn "无法调整 net.ipv4.conf.all.rp_filter（/proc/sys 只读？），若为严格模式可能误报"
    fi
  fi
  if [[ -r "$base/default/rp_filter" ]]; then
    RP_DEFAULT_SAVE=$(cat "$base/default/rp_filter" 2>/dev/null || true)
    echo 0 > "$base/default/rp_filter" 2>/dev/null || true
  fi
}

restore_rp_filter() {
  local base=/proc/sys/net/ipv4/conf
  if [[ -n "$RP_ALL_SAVE" ]]; then
    echo "$RP_ALL_SAVE" > "$base/all/rp_filter" 2>/dev/null || true
  fi
  if [[ -n "$RP_DEFAULT_SAVE" ]]; then
    echo "$RP_DEFAULT_SAVE" > "$base/default/rp_filter" 2>/dev/null || true
  fi
}

# ---------- 辅助函数 ----------
# 选空闲端口：默认 17800，被占用则 +1 重试（最多试 50 个）；用 bash 内建 /dev/tcp 探测
pick_port() {
  local port=$BASE_PORT
  while (( port < BASE_PORT + 50 )); do
    if ! (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      echo "$port"
      return 0
    fi
    port=$(( port + 1 ))
  done
  return 1
}

# 等待 127.0.0.1:port 可连接（coord 就绪），同时监测 coord 进程是否存活
wait_port() {
  local port=$1 pid=$2 i
  for i in $(seq 1 60); do          # 最多约 12 秒
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      return 0
    fi
    if ! kill -0 "$pid" 2>/dev/null; then
      return 1
    fi
    sleep 0.2
  done
  return 1
}

# 从 `ip -4 addr show` 解析某 TUN 网卡上的 10.66.0.x 虚拟 IP（没有则输出空）
iface_vip() {
  local out
  out=$(ip -4 -o addr show dev "$1" 2>/dev/null \
        | awk '$4 ~ /^10\.66\.0\.[0-9]+\/[0-9]+$/ { sub(/\/.*/, "", $4); print $4; exit }') || true
  echo "$out"
}

# 轮询等待虚拟 IP 就绪；同时监测 peer 进程存活。成功则输出 VIP，失败返回非 0
wait_for_vip() {
  local iface=$1 pid=$2 elapsed=0 vip=""
  while (( elapsed < VIP_WAIT_TIMEOUT )); do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "[selftest][FAIL] peer 进程(PID=${pid})已退出，${iface} 未获得虚拟 IP" >&2
      return 1
    fi
    vip=$(iface_vip "$iface")
    if [[ -n "$vip" ]]; then
      echo "$vip"
      return 0
    fi
    sleep 1
    elapsed=$(( elapsed + 1 ))
  done
  return 1
}

# ---------- 主流程 ----------
main() {
  local c abs_bin

  BIN=${1:-./lanparty}
  if abs_bin=$(readlink -f -- "$BIN" 2>/dev/null) && [[ -n "$abs_bin" ]]; then
    BIN=$abs_bin
  fi
  if [[ ! -x "$BIN" ]]; then
    echo "[selftest][FAIL] 二进制不存在或不可执行：$BIN" >&2
    exit 1
  fi
  if [[ ${EUID} -ne 0 ]]; then
    echo "[selftest][FAIL] 请用 sudo/root 运行：peer 创建 TUN 与添加路由需要 root" >&2
    exit 1
  fi
  for c in openssl ip ping; do
    if ! command -v "$c" >/dev/null 2>&1; then
      echo "[selftest][FAIL] 缺少依赖命令：$c" >&2
      exit 1
    fi
  done

  # ---------- a. 打印系统信息 ----------
  log "===== 系统信息 ====="
  uname -a || true
  echo "CPU 核数: $(nproc 2>/dev/null || echo 未知)"
  free -h 2>/dev/null || free 2>/dev/null || true
  if [[ -e /dev/net/tun ]]; then
    log "/dev/net/tun: 存在"
  else
    modprobe tun 2>/dev/null || true
    if [[ ! -e /dev/net/tun ]]; then
      fail "/dev/net/tun 不存在（且 modprobe tun 失败），本机无法进行 TUN 自测"
    fi
    log "/dev/net/tun: 原本不存在，modprobe tun 后已就绪"
  fi
  log "===================="

  # ---------- b. 随机网络名/PSK，选空闲端口 ----------
  NET_NAME="selftest-$(openssl rand -hex 12)"
  PSK=$(openssl rand -hex 12)
  if ! PORT=$(pick_port); then
    fail "端口 ${BASE_PORT}-$(( BASE_PORT + 49 )) 全部被占用，无法启动自测 coord"
  fi
  WORKDIR=$(mktemp -d /tmp/lanparty-selftest.XXXXXX)
  LOG_COORD="$WORKDIR/coord.log"
  LOG_P0="$WORKDIR/peer-${IFACE0}.log"
  LOG_P1="$WORKDIR/peer-${IFACE1}.log"
  log "随机网络名: ${NET_NAME}；PSK: ${PSK:0:4}****（已脱敏）"
  log "自测端口: ${PORT}；临时目录: ${WORKDIR}"

  # 先放宽 rp_filter，保证之后 peer 创建的 TUN 也继承宽松策略（退出时恢复）
  loosen_rp_filter

  # ---------- c. 后台启动 coord，日志到临时文件 ----------
  log "启动 coord --listen 127.0.0.1:${PORT}"
  "$BIN" coord --network "$NET_NAME" --psk "$PSK" --listen "127.0.0.1:${PORT}" >"$LOG_COORD" 2>&1 &
  COORD_PID=$!
  if ! wait_port "$PORT" "$COORD_PID"; then
    fail "coord 端口 ${PORT} 始终未就绪，或 coord 进程提前退出（PID=${COORD_PID}）"
  fi
  log "coord 已就绪（PID=${COORD_PID}）"

  # ---------- d. 后台启动两个 peer（同机双 TUN），等待虚拟 IP ----------
  # peer 在 Linux 上创建 TUN 需要 root；本脚本已整体以 root 运行
  "$BIN" peer --coord "127.0.0.1:${PORT}" --network "$NET_NAME" --psk "$PSK" \
       --name node-0 --iface "$IFACE0" >"$LOG_P0" 2>&1 &
  P0_PID=$!
  "$BIN" peer --coord "127.0.0.1:${PORT}" --network "$NET_NAME" --psk "$PSK" \
       --name node-1 --iface "$IFACE1" >"$LOG_P1" 2>&1 &
  P1_PID=$!
  log "两个 peer 已启动: node-0(PID=${P0_PID}, iface=${IFACE0}) / node-1(PID=${P1_PID}, iface=${IFACE1})"

  if ! VIP_T0=$(wait_for_vip "$IFACE0" "$P0_PID"); then
    fail "等待 ${IFACE0} 的 10.66.0.x 虚拟 IP 超时（${VIP_WAIT_TIMEOUT}s）"
  fi
  if ! VIP_T1=$(wait_for_vip "$IFACE1" "$P1_PID"); then
    fail "等待 ${IFACE1} 的 10.66.0.x 虚拟 IP 超时（${VIP_WAIT_TIMEOUT}s）"
  fi
  log "${IFACE0} 虚拟 IP = ${VIP_T0}；${IFACE1} 虚拟 IP = ${VIP_T1}"
  if [[ "$VIP_T0" == "$VIP_T1" ]]; then
    fail "两块 TUN 拿到相同虚拟 IP（${VIP_T0}），地址分配异常"
  fi

  # ---------- e. 添加 /32 主机路由，消除同机双网卡的 /24 路由歧义 ----------
  # 两块 TUN 同在 10.66.0.0/24，内核对目的地址的出口选择有歧义；
  # 用 /32 主机路由把“对端 VIP”明确指到“本端 TUN”。
  if ! ip route replace "${VIP_T1}/32" dev "$IFACE0" 2>/dev/null; then
    warn "ip route replace ${VIP_T1}/32 dev ${IFACE0} 失败（可能有等价路由），继续测试"
  fi
  if ! ip route replace "${VIP_T0}/32" dev "$IFACE1" 2>/dev/null; then
    warn "ip route replace ${VIP_T0}/32 dev ${IFACE1} 失败（可能有等价路由），继续测试"
  fi

  # ---------- f. ping 连通性（绑定 -I 保证从指定 TUN 收发） ----------
  log "测试1: ping -c 5 -W 2 -I ${IFACE0} ${VIP_T1}（标准小包）"
  if PING1_OUT=$(ping -c 5 -W 2 -I "$IFACE0" "$VIP_T1" 2>&1); then
    PING1="PASS"
  else
    PING1="FAIL"
  fi
  echo "${PING1_OUT:-（无输出）}"
  grep -E '^(rtt|round-trip)' <<<"$PING1_OUT" || true

  log "测试2: ping -c 3 -s 1200 -I ${IFACE0} ${VIP_T1}（模拟游戏大包）"
  if PING2_OUT=$(ping -c 3 -s 1200 -W 2 -I "$IFACE0" "$VIP_T1" 2>&1); then
    PING2="PASS"
  else
    PING2="FAIL"
  fi
  echo "${PING2_OUT:-（无输出）}"
  grep -E '^(rtt|round-trip)' <<<"$PING2_OUT" || true

  # ---------- h. 汇总 PASS/FAIL ----------
  if [[ "$PING1" == "PASS" && "$PING2" == "PASS" ]]; then
    OVERALL_ALL="PASS"
    log "全部测试通过"
  else
    warn "连通性测试未通过，以下输出 coord/peer 日志与网络现场供排查"
    dump_logs
    dump_state
  fi
  finish
  if [[ "$OVERALL_ALL" == "PASS" ]]; then
    exit 0
  else
    exit 1
  fi
}

trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

main "$@"
