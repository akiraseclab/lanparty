//go:build windows

// Windows 上的虚拟网卡：wintun 驱动 + netsh 配置地址。
// 前置条件：以管理员身份运行；wintun.dll 与 exe 同目录（见 README）。
package tun

import (
	"fmt"
	"net/netip"
	"os/exec"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

func init() { PlatformCreate = create }

func create(name string, mtu int, vip netip.Addr, prefix netip.Prefix) (Device, error) {
	dev, err := wgtun.CreateTUN(name, mtu)
	if err != nil {
		return nil, fmt.Errorf(
			"创建 wintun 虚拟网卡失败（需要：1) 以管理员身份运行；2) wintun.dll 与 exe 同目录，下载地址见 README）: %w", err)
	}
	mask := "255.255.255.0" // v0.1 只支持 /24 子网
	cmd := exec.Command("netsh", "interface", "ip", "set", "address",
		"name="+name, "source=static", "addr="+vip.String(), "mask="+mask)
	if out, err := cmd.CombinedOutput(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("netsh 配置网卡地址失败: %v: %s", err, out)
	}
	return wrap(dev, mtu, 0)
}
