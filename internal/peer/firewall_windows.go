//go:build windows

// Windows 防火墙自动放行：新建的 TUN 网卡会被归为"公用网络"，默认拦截
// 入站 ICMP 与游戏端口，症状是"能连上协调节点但互 ping 不通"。这里在
// 启动时幂等地添加一条只对虚拟网段放行的入站规则（需要管理员权限，
// peer 创建 TUN 本身就需要，因此时机刚好）。
package peer

import (
	"log/slog"
	"os/exec"
	"strings"
)

const firewallRuleName = "lanparty-in"

func ensureFirewallRule() {
	out, err := exec.Command("netsh", "advfirewall", "firewall", "show", "rule",
		"name="+firewallRuleName).Output()
	if err == nil && strings.Contains(string(out), firewallRuleName) {
		return // 已存在，幂等返回
	}
	if out, err := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name="+firewallRuleName, "dir=in", "action=allow",
		"remoteip=10.66.0.0/24").CombinedOutput(); err != nil {
		slog.Warn("添加防火墙放行规则失败（不影响隧道，但队友可能 ping 不到你）",
			"err", err, "output", strings.TrimSpace(string(out)))
	} else {
		slog.Info("已添加虚拟网段入站放行规则", "rule", firewallRuleName, "scope", "10.66.0.0/24")
	}
}
