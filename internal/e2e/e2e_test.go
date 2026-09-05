// Package e2e 用内存虚拟网卡跑通"协调节点 + 两个成员"的完整链路，
// 不触碰真实系统网络，可在任何环境（包括 CI）运行。
package e2e

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"lanparty/internal/coord"
	"lanparty/internal/peer"
	"lanparty/internal/tun"
)

// testPacket 构造一个合法 IPv4 报文（协议号 253 为实验用途）。
func testPacket(src, dst netip.Addr, payload []byte) []byte {
	total := 20 + len(payload)
	pkt := make([]byte, total)
	pkt[0] = 0x45 // v4, IHL=5
	binary.BigEndian.PutUint16(pkt[2:4], uint16(total))
	pkt[8] = 64 // TTL
	pkt[9] = 253
	s := src.As4()
	d := dst.As4()
	copy(pkt[12:16], s[:])
	copy(pkt[16:20], d[:])
	copy(pkt[20:], payload)
	sum := checksum(pkt[:20])
	pkt[10] = byte(sum >> 8)
	pkt[11] = byte(sum)
	return pkt
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func startCoord(t *testing.T) *coord.Coordinator {
	t.Helper()
	c, err := coord.Parse(coord.Options{
		Listen:   "127.0.0.1:0",
		Networks: map[string]coord.NetworkConfig{"test": {PSK: "secret", Subnet: "10.66.0.0/24"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	go c.Serve()
	t.Cleanup(c.Close)
	return c
}

// joinPeer 让一个内存虚拟网卡的成员加入网络，返回它的虚拟网卡与分配到的 VIP。
func joinPeer(t *testing.T, ctx context.Context, c *coord.Coordinator, name, psk string) (*tun.MemDevice, netip.Addr) {
	t.Helper()
	dev := tun.NewMem(1400)
	vipCh := make(chan netip.Addr, 1)
	factory := func(vip netip.Addr, prefix netip.Prefix, mtu int) (tun.Device, error) {
		vipCh <- vip
		return dev, nil
	}
	p := peer.New(peer.Options{
		Coordinator: c.Addr().String(),
		Network:     "test",
		PSK:         psk,
		Name:        name,
	}, factory)
	go p.Run(ctx)
	select {
	case vip := <-vipCh:
		return dev, vip
	case <-time.After(5 * time.Second):
		t.Fatalf("%s 加入网络超时", name)
		return nil, netip.Addr{}
	}
}

func TestTwoPeersRelay(t *testing.T) {
	c := startCoord(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	devA, vipA := joinPeer(t, ctx, c, "A", "secret")
	devB, vipB := joinPeer(t, ctx, c, "B", "secret")
	time.Sleep(300 * time.Millisecond) // 等成员表在服务端稳定

	// A -> B
	pkt := testPacket(vipA, vipB, []byte("hello B"))
	if err := devA.FeedOS(pkt); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-devB.Delivered():
		if !bytes.Equal(got, pkt) {
			t.Fatalf("B 收到的报文不一致:\n 期望 %v\n 实际 %v", pkt, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B 没有收到 A 的报文")
	}

	// B -> A
	pkt2 := testPacket(vipB, vipA, []byte("hi A"))
	if err := devB.FeedOS(pkt2); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-devA.Delivered():
		if !bytes.Equal(got, pkt2) {
			t.Fatalf("A 收到的报文不一致:\n 期望 %v\n 实际 %v", pkt2, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("A 没有收到 B 的报文")
	}
}

func TestWrongPSKCannotJoin(t *testing.T) {
	c := startCoord(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dev := tun.NewMem(1400)
	vipCh := make(chan netip.Addr, 1)
	factory := func(vip netip.Addr, _ netip.Prefix, _ int) (tun.Device, error) {
		vipCh <- vip
		return dev, nil
	}
	p := peer.New(peer.Options{
		Coordinator: c.Addr().String(),
		Network:     "test",
		PSK:         "totally-wrong",
		Name:        "intruder",
	}, factory)
	go p.Run(ctx)

	select {
	case vip := <-vipCh:
		t.Fatalf("错误密码竟然拿到了虚拟 IP: %v", vip)
	case <-time.After(800 * time.Millisecond):
		// 预期行为：加不进来
	}
}

func TestUnknownNetworkRejected(t *testing.T) {
	c := startCoord(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dev := tun.NewMem(1400)
	vipCh := make(chan netip.Addr, 1)
	factory := func(vip netip.Addr, _ netip.Prefix, _ int) (tun.Device, error) {
		vipCh <- vip
		return dev, nil
	}
	p := peer.New(peer.Options{
		Coordinator: c.Addr().String(),
		Network:     "no-such-network",
		PSK:         "secret",
		Name:        "lost",
	}, factory)
	go p.Run(ctx)

	select {
	case vip := <-vipCh:
		t.Fatalf("未知网络竟然拿到了虚拟 IP: %v", vip)
	case <-time.After(800 * time.Millisecond):
		// 预期行为：加不进来
	}
}

func TestBroadcastRelay(t *testing.T) {
	c := startCoord(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	devA, vipA := joinPeer(t, ctx, c, "A", "secret")
	devB, _ := joinPeer(t, ctx, c, "B", "secret")
	time.Sleep(300 * time.Millisecond)

	// A 发局域网发现广播（255.255.255.255），B 应收到，A 自己不能收到回声
	bc := testPacket(vipA, netip.AddrFrom4([4]byte{255, 255, 255, 255}), []byte("lan-discovery"))
	if err := devA.FeedOS(bc); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-devB.Delivered():
		if !bytes.Equal(got, bc) {
			t.Fatal("B 收到的广播内容不一致")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B 没有收到 A 的广播")
	}
	select {
	case got := <-devA.Delivered():
		t.Fatalf("A 收到了自己广播的回声（应被过滤）: %v", got)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestVIPMemory(t *testing.T) {
	c := startCoord(t)
	ctx1, cancel1 := context.WithCancel(context.Background())
	_, vip1 := joinPeer(t, ctx1, c, "reconnector", "secret")
	cancel1() // 主动下线
	time.Sleep(time.Second) // 等协调节点处理离席并记忆 IP

	_, vip2 := joinPeer(t, context.Background(), c, "reconnector", "secret")
	if vip1 != vip2 {
		t.Fatalf("IP 记忆失败：首次 %s，重连 %s", vip1, vip2)
	}
}
