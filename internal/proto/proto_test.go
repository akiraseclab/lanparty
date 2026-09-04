package proto

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
)

type serverResult struct {
	hello *Hello
	conn  *Conn
	err   error
}

func runServer(rwc io.ReadWriteCloser, psk string) <-chan serverResult {
	ch := make(chan serverResult, 1)
	go func() {
		h, c, err := ServerHandshake(rwc, 1400, func(*Hello) (string, string, string, error) {
			return psk, "10.66.0.0/24", "10.66.0.2", nil
		})
		ch <- serverResult{hello: h, conn: c, err: err}
	}()
	return ch
}

// bufPipe 是单方向的带缓冲管道。net.Pipe 是同步管道（Write 会阻塞到对端
// Read），而这些测试的写法是"先 Send 后 Receive"，直接用 net.Pipe 会死锁。
type bufPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func (p *bufPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 && !p.closed {
		p.cond.Wait()
	}
	if p.buf.Len() == 0 {
		return 0, io.EOF
	}
	return p.buf.Read(b)
}

func (p *bufPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}
	p.buf.Write(b)
	p.cond.Broadcast()
	return len(b), nil
}

func (p *bufPipe) close() {
	p.mu.Lock()
	p.closed = true
	p.cond.Broadcast()
	p.mu.Unlock()
}

// pipeEnd 把两个方向的缓冲拼成一条双工连接。
type pipeEnd struct{ r, w *bufPipe }

func (e *pipeEnd) Read(b []byte) (int, error)  { return e.r.Read(b) }
func (e *pipeEnd) Write(b []byte) (int, error) { return e.w.Write(b) }
func (e *pipeEnd) Close() error                { e.r.close(); e.w.close(); return nil }

func newBufPipePair() (io.ReadWriteCloser, io.ReadWriteCloser) {
	a, b := &bufPipe{}, &bufPipe{}
	a.cond, b.cond = sync.NewCond(&a.mu), sync.NewCond(&b.mu)
	return &pipeEnd{r: b, w: a}, &pipeEnd{r: a, w: b}
}

func TestHandshakeAndFrames(t *testing.T) {
	c1, c2 := newBufPipePair()
	res := runServer(c2, "secret")

	w, client, err := ClientHandshake(c1, "mynet", "secret", "alice", "test")
	if err != nil {
		t.Fatalf("客户端握手失败: %v", err)
	}
	if w.VIP != "10.66.0.2" || w.Subnet != "10.66.0.0/24" || w.MTU != 1400 {
		t.Fatalf("Welcome 内容异常: %+v", w)
	}
	sr := <-res
	if sr.err != nil {
		t.Fatalf("服务端握手失败: %v", sr.err)
	}
	if sr.hello.Name != "alice" || sr.hello.Network != "mynet" {
		t.Fatalf("Hello 内容异常: %+v", sr.hello)
	}

	// 客户端 -> 服务器
	body := PacketBody([4]byte{10, 66, 0, 2}, [4]byte{10, 66, 0, 3}, []byte("payload-1"))
	if err := client.Send(FramePacket, body); err != nil {
		t.Fatal(err)
	}
	ft, got, err := sr.conn.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if ft != FramePacket || !bytes.Equal(got, body) {
		t.Fatalf("帧内容不一致: type=%d body=%v", ft, got)
	}

	// 服务器 -> 客户端
	if err := sr.conn.Send(FramePong, []byte("pong-data")); err != nil {
		t.Fatal(err)
	}
	ft, got, err = client.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if ft != FramePong || string(got) != "pong-data" {
		t.Fatalf("Pong 不一致: type=%d body=%v", ft, got)
	}
}

func TestWrongPSKRejected(t *testing.T) {
	c1, c2 := newBufPipePair()
	res := runServer(c2, "right-psk")

	// 客户端用错误 PSK：握手本身能"完成"（密钥派生自错误 PSK），
	// 但服务端解第一帧时必然失败 —— 这就是 PSK 认证生效的方式。
	_, client, err := ClientHandshake(c1, "mynet", "wrong-psk", "bob", "test")
	if err != nil {
		t.Fatalf("握手阶段不应报错: %v", err)
	}
	sr := <-res
	if sr.err != nil {
		t.Fatalf("服务端握手不应报错: %v", sr.err)
	}
	_ = client.Send(FramePing, []byte("x"))
	_, _, err = sr.conn.Receive()
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("期望 ErrAuth，得到: %v", err)
	}
	// 服务端随后关闭连接，客户端读到错误
	sr.conn.Close()
	if _, _, err := client.Receive(); err == nil {
		t.Fatal("客户端应当收到连接关闭错误")
	}
}

func TestPacketCodec(t *testing.T) {
	src := [4]byte{10, 66, 0, 2}
	dst := [4]byte{10, 66, 0, 3}
	body := PacketBody(src, dst, []byte("abc"))
	gotSrc, gotDst, payload, err := ParsePacket(body)
	if err != nil || gotSrc != src || gotDst != dst || string(payload) != "abc" {
		t.Fatalf("codec 往返失败: %v %v %v %v", gotSrc, gotDst, payload, err)
	}
	if _, _, _, err := ParsePacket([]byte{1, 2, 3}); err == nil {
		t.Fatal("过短的 body 应当报错")
	}
}

func TestMembersCodec(t *testing.T) {
	list := []MemberInfo{{VIP: "10.66.0.2", Name: "a"}, {VIP: "10.66.0.3", Name: "b"}}
	got, err := ParseMembers(MembersBody(list))
	if err != nil || len(got) != 2 || got[0].VIP != "10.66.0.2" || got[1].Name != "b" {
		t.Fatalf("成员列表编解码失败: %v %v", got, err)
	}
}
