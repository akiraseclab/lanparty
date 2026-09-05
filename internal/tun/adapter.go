//go:build windows || linux

// 平台真实虚拟网卡的公共适配层：把 wireguard-go 的批量读写接口
// 包成 lanparty 的单包 Device 接口。
//
// 关键坑：wireguard-go 批量接口的 offset 是"调用方预留的头部空间"——
//   - Linux：CreateTUN 无条件启用 IFF_VNET_HDR（virtio/GRO），要求
//     offset >= 10（virtioNetHdrLen），且缓冲区前 10 字节是它写 virtio 头的位置；
//     offset=0 会直接报 "invalid offset"（v0.1 真机自测踩过的坑）。
//   - Windows：wintun 无此要求，约定 offset=0。
// 适配层把这些差异消化在内部，对外永远暴露"裸 IP 报文"。
package tun

import (
	"fmt"

	wgtun "golang.zx2c4.com/wireguard/tun"
)

// linuxTunHeadroom = wireguard-go 的 virtioNetHdrLen（源码私有，这里按值固化）。
const linuxTunHeadroom = 10

// RealDevice 基于 WireGuard 项目的 TUN 实现（Windows=wintun 驱动，Linux=内核 TUN）。
// Read 只允许单个协程调用（peer 的出站泵），scratch 缓冲因此无竞争。
type RealDevice struct {
	dev     wgtun.Device
	mtu     int
	offset  int
	scratch []byte
}

func wrap(dev wgtun.Device, mtu, offset int) (Device, error) {
	return &RealDevice{
		dev:     dev,
		mtu:     mtu,
		offset:  offset,
		scratch: make([]byte, offset+mtu),
	}, nil
}

func (d *RealDevice) Read(packet []byte) (int, error) {
	sizes := []int{0}
	if _, err := d.dev.Read([][]byte{d.scratch}, sizes, d.offset); err != nil {
		return 0, err
	}
	// sizes[0] 是不含 offset 头部的报文长度（两种平台路径语义一致）
	if sizes[0] <= 0 || sizes[0] > d.mtu {
		return 0, fmt.Errorf("tun: 报文长度异常 %d", sizes[0])
	}
	return copy(packet, d.scratch[d.offset:d.offset+sizes[0]]), nil
}

func (d *RealDevice) Write(packet []byte) (int, error) {
	buf := make([]byte, d.offset+len(packet))
	copy(buf[d.offset:], packet)
	if _, err := d.dev.Write([][]byte{buf}, d.offset); err != nil {
		return 0, err
	}
	return len(packet), nil
}

func (d *RealDevice) Close() error { return d.dev.Close() }

func (d *RealDevice) MTU() int { return d.mtu }
