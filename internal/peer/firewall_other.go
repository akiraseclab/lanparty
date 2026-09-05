//go:build !windows

// 非 Windows 平台没有对应的防火墙拦截问题（Linux iptables/ufw 由用户自行
// 管理入站策略），空实现保持接口一致。
package peer

func ensureFirewallRule() {}
