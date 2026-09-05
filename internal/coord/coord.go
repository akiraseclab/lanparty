// Package coord 实现中心协调节点（跑在有公网 IP 的服务器上）：
// 分配虚拟 IP、维护成员表、转发成员之间的 IP 报文。
//
// v0.1 的所有流量都经过协调节点中转——对"国内服务器中转联机"场景完全够用；
// 成员之间的 P2P 直连打洞是 v0.2 的目标（见 README 路线图）。
package coord

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"sync"
	"time"

	"lanparty/internal/proto"
)

const (
	HandshakeTimeout = 10 * time.Second
	ReadIdleTimeout  = 180 * time.Second // 超过该时长无任何帧则断开（客户端有 15s 心跳）
	DefaultMTU       = 1400
)

// NetworkConfig 描述一个虚拟网络。
type NetworkConfig struct {
	PSK    string `json:"psk"`    // 预共享密钥（全员一致）
	Subnet string `json:"subnet"` // 虚拟子网，v0.1 仅支持 IPv4 /24，如 10.66.0.0/24
}

// Options 协调节点配置，与 coord.json 对应。
type Options struct {
	Listen   string                   `json:"listen"`
	Networks map[string]NetworkConfig `json:"networks"`
}

type network struct {
	cfg     NetworkConfig
	prefix  netip.Prefix
	mu      sync.Mutex
	members map[netip.Addr]*member
	used    map[netip.Addr]bool
	next    uint8   // 下一次尝试分配的主机位（从 2 开始，1 留给协调节点习惯位）
	lastVIP map[string]netip.Addr // 按成员名记忆上次的虚拟 IP（进程内，重连不变）
}

type member struct {
	vip  netip.Addr
	name string
	conn *proto.Conn
}

// Coordinator 是协调节点实例。
type Coordinator struct {
	ln     net.Listener
	nets   map[string]*network
	closed chan struct{}
}

// Parse 校验配置并开始监听。
func Parse(opts Options) (*Coordinator, error) {
	c := &Coordinator{closed: make(chan struct{}), nets: map[string]*network{}}
	for name, cfg := range opts.Networks {
		prefix, err := netip.ParsePrefix(cfg.Subnet)
		if err != nil {
			return nil, fmt.Errorf("网络 %q 的子网格式错误: %w", name, err)
		}
		if !prefix.Addr().Is4() || prefix.Bits() != 24 {
			return nil, fmt.Errorf("网络 %q 的子网必须是 IPv4 /24（如 10.66.0.0/24）", name)
		}
		if cfg.PSK == "" {
			return nil, fmt.Errorf("网络 %q 未设置 psk", name)
		}
		c.nets[name] = &network{
			cfg: cfg, prefix: prefix,
			members: map[netip.Addr]*member{},
			used:    map[netip.Addr]bool{},
			lastVIP: map[string]netip.Addr{},
			next:    2,
		}
	}
	if len(c.nets) == 0 {
		return nil, errors.New("至少需要配置一个网络（--network + --psk 或配置文件）")
	}
	ln, err := net.Listen("tcp", opts.Listen)
	if err != nil {
		return nil, err
	}
	c.ln = ln
	return c, nil
}

// Addr 返回实际监听地址（测试里用 :0 时可拿到真实端口）。
func (c *Coordinator) Addr() net.Addr { return c.ln.Addr() }

// Close 停止接受新连接（已有连接由各自的 handle 协程自然结束）。
func (c *Coordinator) Close() {
	select {
	case <-c.closed: // 已关闭
	default:
		close(c.closed)
	}
	c.ln.Close()
}

// Serve 阻塞接受连接，每个连接一个协程。
func (c *Coordinator) Serve() error {
	for {
		sock, err := c.ln.Accept()
		if err != nil {
			select {
			case <-c.closed:
				return nil
			default:
				return err
			}
		}
		go c.handle(sock)
	}
}

func (c *Coordinator) handle(sock net.Conn) {
	defer sock.Close()
	sock.SetDeadline(time.Now().Add(HandshakeTimeout))

	st, vip, hello, pconn, err := c.handshake(sock)
	if err != nil {
		if st != nil {
			st.free(vip) // VIP 已分配但认证未完成，释放回池子
		}
		if !errors.Is(err, io.EOF) {
			slog.Warn("握手失败", "remote", sock.RemoteAddr(), "err", err)
		}
		return
	}
	m := &member{vip: vip, name: hello.Name, conn: pconn}
	st.join(m)
	slog.Info("成员上线", "network", hello.Network, "name", hello.Name, "vip", vip, "remote", sock.RemoteAddr())
	defer func() {
		st.leave(m)
		slog.Info("成员下线", "network", hello.Network, "name", hello.Name, "vip", vip)
	}()
	st.broadcast()

	sock.SetDeadline(time.Time{})
	for {
		sock.SetDeadline(time.Now().Add(ReadIdleTimeout))
		ftype, body, err := pconn.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, proto.ErrAuth) {
				slog.Debug("连接读取结束", "vip", vip, "err", err)
			}
			return
		}
		switch ftype {
		case proto.FramePacket:
			src, dst, _, err := proto.ParsePacket(body)
			if err != nil {
				continue
			}
			// 源地址校验：不允许伪造他人的虚拟 IP（防搅浑水）
			if netip.AddrFrom4(src) != vip {
				slog.Warn("报文源地址与分配的 VIP 不符，丢弃", "claimed", netip.AddrFrom4(src), "real", vip)
				continue
			}
			st.forward(vip, dst, body)
		case proto.FrameBroadcast:
			src, _, _, err := proto.ParsePacket(body)
			if err != nil {
				continue
			}
			if netip.AddrFrom4(src) != vip {
				slog.Warn("广播源地址与分配的 VIP 不符，丢弃", "claimed", netip.AddrFrom4(src), "real", vip)
				continue
			}
			st.flood(vip, body)
		case proto.FramePing:
			pconn.Send(proto.FramePong, body)
		case proto.FrameBye:
			return
		}
	}
}

// handshake 完成协议握手；即使返回 err，st/vip 也可能非零（用于释放）。
func (c *Coordinator) handshake(sock net.Conn) (*network, netip.Addr, *proto.Hello, *proto.Conn, error) {
	var st *network
	var vip netip.Addr
	hello, pconn, err := proto.ServerHandshake(sock, DefaultMTU, func(h *proto.Hello) (psk, subnet, vipStr string, err error) {
		s := c.nets[h.Network]
		if s == nil {
			return "", "", "", fmt.Errorf("未知网络 %q", h.Network)
		}
		addr, aerr := s.allocate(h.Name)
		if aerr != nil {
			return "", "", "", aerr
		}
		st, vip = s, addr
		return s.cfg.PSK, s.prefix.String(), addr.String(), nil
	})
	if err != nil {
		return st, vip, nil, nil, err
	}
	return st, vip, hello, pconn, nil
}

// ---------- network 的成员与 IP 管理 ----------

// allocate 分配虚拟 IP：优先归还同名成员上次用过的（IP 记忆，重连不变），
// 否则扫描空闲主机位（跳过网络地址、广播地址与 .1）。
func (n *network) allocate(name string) (netip.Addr, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if last, ok := n.lastVIP[name]; ok && !n.used[last] {
		n.used[last] = true
		return last, nil
	}
	base := n.prefix.Masked().Addr().As4()
	for i := 0; i < 253; i++ {
		n.next++
		if n.next == 255 {
			n.next = 2
		}
		addr := netip.AddrFrom4([4]byte{base[0], base[1], base[2], n.next})
		if !n.used[addr] {
			n.used[addr] = true
			return addr, nil
		}
	}
	return netip.Addr{}, fmt.Errorf("虚拟网络已满（/24 最多 253 个成员）")
}

func (n *network) free(addr netip.Addr) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.used, addr)
	delete(n.members, addr)
}

func (n *network) join(m *member) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.members[m.vip] = m
}

func (n *network) leave(m *member) {
	n.mu.Lock()
	delete(n.members, m.vip)
	delete(n.used, m.vip)
	n.lastVIP[m.name] = m.vip // 记忆，供同名成员重连时归还
	n.mu.Unlock()
	n.broadcast()
}

// forward 把报文转给目标成员。注意：Send 不在锁内，避免慢连接阻塞整个网络。
func (n *network) forward(from netip.Addr, dst [4]byte, body []byte) {
	n.mu.Lock()
	m := n.members[netip.AddrFrom4(dst)]
	n.mu.Unlock()
	if m == nil || m.vip == from {
		return // 目标不存在或是发给自己，直接丢弃
	}
	m.conn.Send(proto.FramePacket, body) // 发送失败等读循环自己发现并清理
}

// flood 把广播/组播报文分发给除发送者外的所有成员。
// 星型拓扑只有协调节点一个分发点，天然无环；转发为 FramePacket，
// 客户端收到后当作普通入站报文注入 TUN，无需感知广播语义。
func (n *network) flood(from netip.Addr, body []byte) {
	n.mu.Lock()
	targets := make([]*member, 0, len(n.members))
	for _, m := range n.members {
		if m.vip != from {
			targets = append(targets, m)
		}
	}
	n.mu.Unlock()
	for _, m := range targets {
		m.conn.Send(proto.FramePacket, body)
	}
}

// snapshot 返回排序后的成员列表。
func (n *network) snapshot() []proto.MemberInfo {
	n.mu.Lock()
	defer n.mu.Unlock()
	list := make([]proto.MemberInfo, 0, len(n.members))
	for _, m := range n.members {
		list = append(list, proto.MemberInfo{VIP: m.vip.String(), Name: m.name})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].VIP < list[j].VIP })
	return list
}

// broadcast 把最新成员表推给所有在线成员。
func (n *network) broadcast() {
	list := n.snapshot()
	if len(list) == 0 {
		return
	}
	body := proto.MembersBody(list)
	n.mu.Lock()
	targets := make([]*member, 0, len(n.members))
	for _, m := range n.members {
		targets = append(targets, m)
	}
	n.mu.Unlock()
	for _, m := range targets {
		m.conn.Send(proto.FrameMembers, body)
	}
}
