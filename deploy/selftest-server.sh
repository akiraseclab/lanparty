#!/usr/bin/env bash
# lanparty 服务器真机自测：用网络命名空间隔离两个成员，等价于两台真实机器互联。
#
# 用法: sudo ./selftest-server.sh [lanparty二进制路径，默认 ./lanparty]
#
# 原理：coord 跑在根命名空间，两个 peer 分别跑在 nsA/nsB（各自拥有独立的
# 内核网络栈和 TUN 网卡），经 veth 连回根命名空间访问 coord。从 nsA ping
# nsB 的虚拟 IP，流量完整经过 TUN -> peer -> coord -> peer -> TUN 全链路。
#
# 注意：不要在两个成员共用同一内核网络栈的情况下测试（如同一台机不隔离直接
# 双 peer），内核会发现目标是本机地址而把流量走 lo，永远打不进 TUN。
set -uo pipefail

BIN="${1:-./lanparty}"
NS_A="lanparty-st-a"
NS_B="lanparty-st-b"
UNIT_COORD="lanparty-selftest-coord"
UNIT_P0="lanparty-selftest-p0"
UNIT_P1="lanparty-selftest-p1"
PORT=17800
PASS_ALL=0

log() { echo "[selftest] $*"; }
fail() { echo "[selftest][FAIL] $*" >&2; PASS_ALL=1; }

cleanup() {
	trap - EXIT INT TERM
	systemctl stop "$UNIT_COORD" "$UNIT_P0" "$UNIT_P1" 2>/dev/null
	ip netns del "$NS_A" 2>/dev/null
	ip netns del "$NS_B" 2>/dev/null
	echo "[selftest] 清理完成（服务已停止、命名空间已删除，无残留）"
}
trap cleanup EXIT INT TERM

[[ $EUID -eq 0 ]] || { echo "需要 root（创建 TUN 与网络命名空间）"; exit 1; }
[[ -x $BIN ]] || { echo "二进制不存在或不可执行: $BIN"; exit 1; }
command -v systemctl >/dev/null || { echo "需要 systemd"; exit 1; }
command -v ip >/dev/null || { echo "缺少 ip 命令"; exit 1; }
[[ -e /dev/net/tun ]] || { echo "缺少 /dev/net/tun（内核未启用 TUN）"; exit 1; }

IP_BIN="$(command -v ip)"

log "===== 系统信息 ====="
uname -srmo
echo "CPU: $(nproc) 核；内存: $(free -h | awk '/Mem:/{print $2}')；TUN: $(ls -l /dev/net/tun >/dev/null 2>&1 && echo 存在 || echo 缺失)"
log "===================="

systemctl stop "$UNIT_COORD" "$UNIT_P0" "$UNIT_P1" 2>/dev/null

NET="selftest-$(openssl rand -hex 6 2>/dev/null || head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')"
PSK="$(openssl rand -hex 16)"
while ss -tln | grep -q ":${PORT}\b"; do
	PORT=$((PORT + 1))
	((PORT > 17850)) && { echo "17800-17850 无可用端口"; exit 1; }
done

# ---- 命名空间与 veth（192.168.251.0/30 与 .4/30，避开常见网段）----
ip netns add "$NS_A"; ip netns add "$NS_B"
ip link add vstA-root type veth peer name vstA-ns; ip link set vstA-ns netns "$NS_A"
ip addr add 192.168.251.1/30 dev vstA-root; ip link set vstA-root up
ip netns exec "$NS_A" ip addr add 192.168.251.2/30 dev vstA-ns
ip netns exec "$NS_A" ip link set vstA-ns up
ip netns exec "$NS_A" ip link set lo up
ip link add vstB-root type veth peer name vstB-ns; ip link set vstB-ns netns "$NS_B"
ip addr add 192.168.251.5/30 dev vstB-root; ip link set vstB-root up
ip netns exec "$NS_B" ip addr add 192.168.251.6/30 dev vstB-ns
ip netns exec "$NS_B" ip link set vstB-ns up
ip netns exec "$NS_B" ip link set lo up

systemd-run --unit="$UNIT_COORD" "$BIN" coord --network "$NET" --psk "$PSK" --listen "0.0.0.0:${PORT}" >/dev/null
systemd-run --unit="$UNIT_P0" "$IP_BIN" netns exec "$NS_A" "$BIN" peer --coord "192.168.251.1:${PORT}" --network "$NET" --psk "$PSK" --name node-0 --iface lanparty-t0 >/dev/null
systemd-run --unit="$UNIT_P1" "$IP_BIN" netns exec "$NS_B" "$BIN" peer --coord "192.168.251.5:${PORT}" --network "$NET" --psk "$PSK" --name node-1 --iface lanparty-t1 >/dev/null

# ---- 等待两端拿到虚拟 IP（最长 20s）----
vip_of() { # $1=命名空间 $2=网卡名
	for _ in $(seq 1 20); do
		v="$(ip netns exec "$1" ip -4 addr show dev "$2" 2>/dev/null | grep -oP 'inet \K10\.66\.0\.\d+' || true)"
		[[ -n $v ]] && { echo "$v"; return 0; }
		sleep 1
	done
	return 1
}
VIP0="$(vip_of "$NS_A" lanparty-t0)" || fail "node-0 未在 20s 内拿到虚拟 IP"
VIP1="$(vip_of "$NS_B" lanparty-t1)" || fail "node-1 未在 20s 内拿到虚拟 IP"
log "node-0 VIP: ${VIP0:-无}；node-1 VIP: ${VIP1:-无}"

# ---- 连通性测试（从 nsA 发往 nsB）----
SMALL_RX="$(ip netns exec "$NS_A" ping -c 5 -W 2 "${VIP1:-10.66.0.4}" 2>/dev/null | awk '/packets transmitted/{print $4}')"
[[ ${SMALL_RX:-0} -ge 4 ]] && log "小包 ping:   OK（5 发 ${SMALL_RX} 收）" || fail "小包 ping 丢包（收 ${SMALL_RX:-0}/5）"

BIG_RX="$(ip netns exec "$NS_A" ping -c 3 -s 1000 -W 2 "${VIP1:-10.66.0.4}" 2>/dev/null | awk '/packets transmitted/{print $4}')"
[[ ${BIG_RX:-0} -eq 3 ]] && log "大包 ping:   OK（3 发 3 收，1000B 模拟游戏流量）" || fail "大包 ping 丢包（收 ${BIG_RX:-0}/3）"

if ((PASS_ALL != 0)); then
	echo "----- node-0 日志（末尾 15 行）-----" >&2
	journalctl -u "$UNIT_P0" --no-pager -o cat | tail -15 >&2
	echo "----- node-1 日志（末尾 15 行）-----" >&2
	journalctl -u "$UNIT_P1" --no-pager -o cat | tail -15 >&2
fi

log "=========================================================="
if ((PASS_ALL == 0)); then
	log "lanparty 真机自测结果: PASS ✔"
else
	log "lanparty 真机自测结果: FAIL ✘"
fi
exit "$PASS_ALL"
