// Package tun 抽象虚拟网卡。
//
// Device 的语义与操作系统 TUN 一致：
//   - Read  = 读取一个"本机协议栈发往虚拟网络"的 IP 报文（出站）
//   - Write = 把一个"虚拟网络送到本机"的 IP 报文注入协议栈（入站）
//
// MemDevice 是纯内存实现，用于单机集成测试，不触碰系统网络。
package tun

import (
	"errors"
	"net/netip"
	"sync"
)

// Device 是虚拟网卡的统一接口，所有平台的真实实现与内存实现都遵守它。
type Device interface {
	Read(packet []byte) (int, error)
	Write(packet []byte) (int, error)
	Close() error
	MTU() int
}

var ErrClosed = errors.New("tun: 设备已关闭")

// PlatformCreate 由平台相关文件（build tag）注入：
// Windows 走 wintun 驱动，Linux 走内核 TUN，其他平台返回"暂不支持"。
// vip/prefix 用来给新网卡配地址，name 是网卡名（如 "lanparty"）。
var PlatformCreate func(name string, mtu int, vip netip.Addr, prefix netip.Prefix) (Device, error)

const memQueue = 256

// MemDevice 内存虚拟网卡。字段语义与真实 TUN 一一对应：
//   - Read()  消费 FeedOS 喂进来的包（模拟协议栈发包经过虚拟网卡）
//   - Write() 收到的包从 Delivered 通道取走（模拟远端包被注入本机协议栈）
type MemDevice struct {
	mtu       int
	readCh    chan []byte
	writeCh   chan []byte
	closeOnce sync.Once
	closed    chan struct{}
}

func NewMem(mtu int) *MemDevice {
	return &MemDevice{
		mtu:     mtu,
		readCh:  make(chan []byte, memQueue),
		writeCh: make(chan []byte, memQueue),
		closed:  make(chan struct{}),
	}
}

func (d *MemDevice) Read(packet []byte) (int, error) {
	select {
	case b := <-d.readCh:
		return copy(packet, b), nil
	case <-d.closed:
		return 0, ErrClosed
	}
}

func (d *MemDevice) Write(packet []byte) (int, error) {
	b := append([]byte(nil), packet...)
	select {
	case d.writeCh <- b:
		return len(packet), nil
	case <-d.closed:
		return 0, ErrClosed
	}
}

// FeedOS 模拟本机协议栈把一个 IP 报文交给虚拟网卡（等待被 Read 读走）。
func (d *MemDevice) FeedOS(packet []byte) error {
	b := append([]byte(nil), packet...)
	select {
	case d.readCh <- b:
		return nil
	case <-d.closed:
		return ErrClosed
	}
}

// Delivered 返回 Write 写入的报文流（模拟注入本机协议栈的远端报文）。
func (d *MemDevice) Delivered() <-chan []byte { return d.writeCh }

func (d *MemDevice) Close() error {
	d.closeOnce.Do(func() { close(d.closed) })
	return nil
}

func (d *MemDevice) MTU() int { return d.mtu }
