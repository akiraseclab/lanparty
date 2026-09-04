//go:build windows || linux

// 平台真实虚拟网卡的公共适配层：把 wireguard-go 的批量读写接口
// 包成 lanparty 的单包 Device 接口。
package tun

import (
	wgtun "golang.zx2c4.com/wireguard/tun"
)

// RealDevice 基于 WireGuard 项目的 TUN 实现（Windows=驱动，Linux=/dev/net/tun）。
type RealDevice struct {
	dev wgtun.Device
	mtu int
}

func (d *RealDevice) Read(packet []byte) (int, error) {
	sizes := []int{0}
	_, err := d.dev.Read([][]byte{packet}, sizes, 0)
	if err != nil {
		return 0, err
	}
	return sizes[0], nil
}

func (d *RealDevice) Write(packet []byte) (int, error) {
	if _, err := d.dev.Write([][]byte{packet}, 0); err != nil {
		return 0, err
	}
	return len(packet), nil
}

func (d *RealDevice) Close() error { return d.dev.Close() }

func (d *RealDevice) MTU() int { return d.mtu }

// wrap 在真实网卡创建成功后统一包装。
func wrap(dev wgtun.Device, mtu int) (Device, error) {
	return &RealDevice{dev: dev, mtu: mtu}, nil
}
