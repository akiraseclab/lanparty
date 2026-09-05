//go:build linux

// Linux 上的虚拟网卡：内核 TUN + ip 命令配置地址（需要 root）。
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
		return nil, fmt.Errorf("创建 TUN 失败（需要 root 权限与 /dev/net/tun）: %w", err)
	}
	run := func(args ...string) error {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	if err := run("ip", "addr", "add", vip.String()+"/24", "dev", name); err != nil {
		dev.Close()
		return nil, fmt.Errorf("配置虚拟网卡地址失败: %w", err)
	}
	if err := run("ip", "link", "set", name, "mtu", fmt.Sprint(mtu), "up"); err != nil {
		dev.Close()
		return nil, fmt.Errorf("启用虚拟网卡失败: %w", err)
	}
	// 10 = virtioNetHdrLen：CreateTUN 启用了 IFF_VNET_HDR，批量接口要求调用方预留
	return wrap(dev, mtu, linuxTunHeadroom)
}
