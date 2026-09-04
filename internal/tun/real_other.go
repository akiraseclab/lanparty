//go:build !windows && !linux

// 其他平台暂未实现真实虚拟网卡（macOS 在路线图中），编译可通过但运行时报错。
package tun

import (
	"errors"
	"net/netip"
)

func init() { PlatformCreate = create }

func create(name string, mtu int, vip netip.Addr, prefix netip.Prefix) (Device, error) {
	return nil, errors.New("lanparty v0.1 仅支持 Windows 与 Linux，macOS 支持在路线图中")
}
