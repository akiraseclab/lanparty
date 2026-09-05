// Package peer 实现成员节点（跑在玩家电脑上）：
// 连接协调节点、创建虚拟网卡、在两者之间搬运 IP 报文。
package peer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"time"

	"lanparty/internal/proto"
	"lanparty/internal/tun"
)

const (
	reconnectMax     = 15 * time.Second
	pingInterval     = 15 * time.Second
	handshakeTimeout = 10 * time.Second
)

// Version 由 main 注入（go build -ldflags "-X lanparty/internal/peer.Version=v1.0.0"）。
var Version = "dev"

// Options 成员节点配置，与 peer.json 对应。
type Options struct {
	Coordinator string `json:"coord"`   // 协调节点地址 host:port
	Network     string `json:"network"` // 网络名称
	PSK         string `json:"psk"`     // 网络密码（与服务器一致）
	Name        string `json:"name"`    // 成员列表里显示的名字（默认主机名）
	Iface       string `json:"iface"`   // 虚拟网卡名（默认 lanparty）
}

// TunFactory 由调用方注入：拿到服务器分配的地址后创建虚拟网卡。
// 真实运行注入平台实现；测试注入内存实现。
type TunFactory func(vip netip.Addr, prefix netip.Prefix, mtu int) (tun.Device, error)

// Peer 是成员节点实例。
type Peer struct {
	opts   Options
	newTun TunFactory
}

func New(opts Options, factory TunFactory) *Peer {
	if opts.Name == "" {
		opts.Name, _ = os.Hostname()
	}
	if opts.Iface == "" {
		opts.Iface = "lanparty"
	}
	return &Peer{opts: opts, newTun: factory}
}

// Run 阻塞运行：断线自动指数退避重连，ctx 取消后返回。
func (p *Peer) Run(ctx context.Context) error {
	ensureFirewallRule() // Windows：自动放行虚拟网段入站（见 firewall_windows.go）
	backoff := time.Second
	for {
		start := time.Now()
		err := p.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second // 刚才稳定连过一段时间，重置退避
		}
		slog.Warn("隧道断开，准备重连", "err", err, "retry_in", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		}
		backoff = min(backoff*2, reconnectMax)
	}
}

// session 完整走一遍会话：拨号 -> 握手 -> 建虚拟网卡 -> 双向泵报文。
func (p *Peer) session(ctx context.Context) error {
	var dialer net.Dialer
	sock, err := dialer.DialContext(ctx, "tcp", p.opts.Coordinator)
	if err != nil {
		return fmt.Errorf("连接协调节点: %w", err)
	}
	defer sock.Close()

	sock.SetDeadline(time.Now().Add(handshakeTimeout))
	welcome, pconn, err := proto.ClientHandshake(sock, p.opts.Network, p.opts.PSK, p.opts.Name, Version)
	if err != nil {
		return err
	}
	vip, err := netip.ParseAddr(welcome.VIP)
	if err != nil {
		return fmt.Errorf("服务器分配的虚拟 IP 无效: %w", err)
	}
	prefix, err := netip.ParsePrefix(welcome.Subnet)
	if err != nil {
		return fmt.Errorf("服务器下发的子网无效: %w", err)
	}
	// PSK 确认：握手阶段不验证 PSK，服务器把成员加进表后会立刻广播成员表，
	// 这第一帧必须能用派生密钥解开——解不开说明 PSK 不匹配。必须在创建
	// 虚拟网卡之前完成这一步，避免错误密码的节点拿到虚拟网卡。
	if _, _, err := pconn.Receive(); err != nil {
		return err
	}
	sock.SetDeadline(time.Time{})
	slog.Info("已加入虚拟网络", "vip", vip, "subnet", prefix, "mtu", welcome.MTU)

	dev, err := p.newTun(vip, prefix, welcome.MTU)
	if err != nil {
		return fmt.Errorf("创建虚拟网卡: %w", err)
	}
	defer dev.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 统一的退出路径：上下文取消时关闭连接，唤醒阻塞在 Receive 上的读循环
	go func() {
		<-runCtx.Done()
		pconn.Close()
	}()

	go p.pingLoop(runCtx, pconn)
	go p.tunToNet(runCtx, dev, pconn, vip, prefix, cancel)
	return p.netToTun(pconn, dev, cancel) // 读循环阻塞；返回即会话结束
}

// tunToNet 出站方向：虚拟网卡 -> 协调节点。
func (p *Peer) tunToNet(ctx context.Context, dev tun.Device, pconn *proto.Conn, vip netip.Addr, prefix netip.Prefix, cancel context.CancelFunc) {
	packet := make([]byte, 65535)
	bcast := broadcastOf(prefix)
	for {
		n, err := dev.Read(packet)
		if err != nil {
			if !errors.Is(err, tun.ErrClosed) {
				slog.Warn("读取虚拟网卡出错", "err", err)
			}
			cancel()
			pconn.Close()
			return
		}
		pkt := packet[:n]
		_, dst, ok := ipv4Parts(pkt)
		// v0.1 只转发虚拟子网内的单播；广播/组播（如 LAN 发现）是路线图功能
		if !ok || dst == vip || dst.IsMulticast() || !prefix.Contains(dst) || dst == bcast {
			continue
		}
		body := proto.PacketBody(vip.As4(), dst.As4(), pkt)
		if err := pconn.Send(proto.FramePacket, body); err != nil {
			cancel()
			pconn.Close()
			return
		}
	}
}

// netToTun 入站方向：协调节点 -> 虚拟网卡。返回即会话结束。
func (p *Peer) netToTun(pconn *proto.Conn, dev tun.Device, cancel context.CancelFunc) error {
	for {
		ftype, body, err := pconn.Receive()
		if err != nil {
			cancel()
			return err
		}
		switch ftype {
		case proto.FramePacket:
			_, _, payload, err := proto.ParsePacket(body)
			if err != nil {
				slog.Warn("收到非法报文帧，丢弃", "err", err)
				continue
			}
			if _, err := dev.Write(payload); err != nil {
				cancel()
				return err
			}
		case proto.FrameMembers:
			if list, err := proto.ParseMembers(body); err == nil && len(list) > 0 {
				names := make([]string, 0, len(list))
				for _, m := range list {
					names = append(names, m.Name+"("+m.VIP+")")
				}
				slog.Info("虚拟网络成员", "count", len(list), "list", strings.Join(names, ", "))
			}
		case proto.FramePing:
			pconn.Send(proto.FramePong, body)
		}
	}
}

func (p *Peer) pingLoop(ctx context.Context, pconn *proto.Conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := pconn.Send(proto.FramePing, []byte("ping")); err != nil {
				return // 连接已坏，读循环会发现并重连
			}
		}
	}
}

// ---------- IPv4 报文辅助 ----------

func ipv4Parts(packet []byte) (src, dst netip.Addr, ok bool) {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return netip.Addr{}, netip.Addr{}, false
	}
	src = netip.AddrFrom4([4]byte(packet[12:16]))
	dst = netip.AddrFrom4([4]byte(packet[16:20]))
	return src, dst, true
}

// broadcastOf 计算 /24 子网的广播地址（如 10.66.0.0/24 -> 10.66.0.255）。
func broadcastOf(prefix netip.Prefix) netip.Addr {
	a := prefix.Masked().Addr().As4()
	a[3] = 255
	return netip.AddrFrom4(a)
}
