// Package proto 定义 lanparty 的线上协议：
//
//	握手阶段（明文 JSON）：Hello -> Welcome
//	数据阶段（加密帧）：  [4字节长度][密文]，密文解开后首字节是帧类型
//
// 加密采用 PSK + HKDF 派生密钥 + AES-256-GCM，双向使用不同密钥；
// PSK 正确性在第一帧解密时得到验证（错误 PSK 会立刻解密失败并被断开）。
package proto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// 加密后的帧类型（密文解开后 body 的首字节）
const (
	FramePing    byte = 0x10 // 保活
	FramePong    byte = 0x11 // 保活应答
	FrameMembers byte = 0x20 // body: 成员列表 JSON（服务器 -> 客户端）
	FramePacket  byte = 0x30 // body: src(4B)+dst(4B)+IP报文
	FrameBye     byte = 0x40 // 主动告别
)

const (
	// MaxPacket 单个 IP 报文的上限，超过直接丢弃（TUN MTU 通常 1400）。
	MaxPacket = 1500
	// MaxFrame 单帧密文上限：报文最大包 + 成员列表等控制帧的富余。
	MaxFrame = 32 * 1024
)

// 握手阶段客户端 -> 服务器
type Hello struct {
	Network string `json:"network"`
	Name    string `json:"name"`
	Version string `json:"version"`
	NonceC  []byte `json:"nonce_c"` // 客户端随机数 16B
}

// 握手阶段服务器 -> 客户端（成功）
type Welcome struct {
	NonceS []byte `json:"nonce_s"` // 服务器随机数 16B
	VIP    string `json:"vip"`     // 分配给客户端的虚拟 IP
	Subnet string `json:"subnet"`  // 虚拟子网，如 10.66.0.0/24
	MTU    int    `json:"mtu"`
}

// 成员信息（FrameMembers 的 JSON 列表元素）
type MemberInfo struct {
	VIP  string `json:"vip"`
	Name string `json:"name"`
}

var (
	ErrAuth     = errors.New("lanparty: 认证失败（PSK 不匹配或数据被篡改）")
	ErrBadFrame = errors.New("lanparty: 非法帧")
)

// ---------- 长度前缀 JSON（仅握手阶段明文使用） ----------

func WriteMsg(w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(data)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func ReadMsg(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > 64*1024 {
		return fmt.Errorf("%w: 握手消息过大", ErrBadFrame)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// ---------- 密钥派生（手工实现 HKDF-SHA256，避免额外依赖） ----------

func hkdfExtract(salt, ikm []byte) []byte {
	h := hmac.New(sha256.New, salt)
	h.Write(ikm)
	return h.Sum(nil)
}

func hkdfExpand(prk []byte, info string, length int) []byte {
	out := make([]byte, 0, length)
	var prev []byte
	for i := byte(1); len(out) < length; i++ {
		h := hmac.New(sha256.New, prk)
		h.Write(prev)
		h.Write([]byte(info))
		h.Write([]byte{i})
		prev = h.Sum(nil)
		out = append(out, prev...)
	}
	return out[:length]
}

// deriveKeys 由 PSK 与双方随机数派生两个方向的密钥（分开派生避免 nonce 复用）。
func deriveKeys(psk, nonceC, nonceS []byte) (c2s, s2c []byte) {
	salt := append(append([]byte{}, nonceC...), nonceS...)
	prk := hkdfExtract(salt, psk)
	return hkdfExpand(prk, "lanparty v1 c2s", 32), hkdfExpand(prk, "lanparty v1 s2c", 32)
}

func randomNonce() []byte {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("lanparty: 系统随机源不可用: " + err.Error())
	}
	return b
}

// ---------- 握手 ----------

// handshakeReply 是服务器的握手应答；Message 非空表示拒绝。
type handshakeReply struct {
	NonceS  []byte `json:"nonce_s"`
	Message string `json:"message,omitempty"`
	VIP     string `json:"vip"`
	Subnet  string `json:"subnet"`
	MTU     int    `json:"mtu"`
}

// ClientHandshake 执行客户端握手：发送 Hello，校验应答，返回虚拟网信息与加密连接。
func ClientHandshake(rwc io.ReadWriteCloser, network, psk, name, version string) (*Welcome, *Conn, error) {
	nonceC := randomNonce()
	if err := WriteMsg(rwc, &Hello{Network: network, Name: name, Version: version, NonceC: nonceC}); err != nil {
		return nil, nil, err
	}
	var reply handshakeReply
	if err := ReadMsg(rwc, &reply); err != nil {
		return nil, nil, err
	}
	if reply.Message != "" {
		return nil, nil, fmt.Errorf("lanparty: 服务器拒绝: %s", reply.Message)
	}
	if len(reply.NonceS) != 16 {
		return nil, nil, fmt.Errorf("%w: 服务器随机数长度错误", ErrBadFrame)
	}
	conn, err := newConn(rwc, []byte(psk), nonceC, reply.NonceS, true)
	if err != nil {
		return nil, nil, err
	}
	return &Welcome{NonceS: reply.NonceS, VIP: reply.VIP, Subnet: reply.Subnet, MTU: reply.MTU}, conn, nil
}

// ServerHandshake 执行服务器握手。assign 回调校验网络名并分配 VIP；
// 注意 PSK 正确性在随后的第一帧解密时才验证（见 Conn.Receive）。
func ServerHandshake(rwc io.ReadWriteCloser, mtu int, assign func(h *Hello) (psk, subnet, vip string, err error)) (*Hello, *Conn, error) {
	var hello Hello
	if err := ReadMsg(rwc, &hello); err != nil {
		return nil, nil, err
	}
	if hello.Network == "" || len(hello.NonceC) != 16 {
		WriteMsg(rwc, &handshakeReply{Message: "bad hello"})
		return nil, nil, fmt.Errorf("%w: hello 不合法", ErrBadFrame)
	}
	psk, subnet, vip, err := assign(&hello)
	if err != nil {
		WriteMsg(rwc, &handshakeReply{Message: err.Error()})
		return nil, nil, err
	}
	nonceS := randomNonce()
	if err := WriteMsg(rwc, &handshakeReply{NonceS: nonceS, VIP: vip, Subnet: subnet, MTU: mtu}); err != nil {
		return nil, nil, err
	}
	conn, err := newConn(rwc, []byte(psk), hello.NonceC, nonceS, false)
	if err != nil {
		return nil, nil, err
	}
	return &hello, conn, nil
}

// ---------- 加密连接 ----------

// Conn 是握手完成后的加密帧通道。
// Send 可被多个协程并发调用；Receive 只允许单个读协程调用。
type Conn struct {
	rwc     io.ReadWriteCloser
	sendKey cipher.AEAD
	recvKey cipher.AEAD
	sendSeq uint64
	recvSeq uint64
	wmu     sync.Mutex
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func newConn(rwc io.ReadWriteCloser, psk, nonceC, nonceS []byte, isClient bool) (*Conn, error) {
	c2s, s2c := deriveKeys(psk, nonceC, nonceS)
	send, recv := c2s, s2c
	if !isClient {
		send, recv = s2c, c2s
	}
	sendKey, err := newAEAD(send)
	if err != nil {
		return nil, err
	}
	recvKey, err := newAEAD(recv)
	if err != nil {
		return nil, err
	}
	return &Conn{rwc: rwc, sendKey: sendKey, recvKey: recvKey}, nil
}

// Send 加密并发送一帧。body 可为 nil。
func (c *Conn) Send(frameType byte, body []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.sendSeq++
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], c.sendSeq)
	plain := make([]byte, 0, 1+len(body))
	plain = append(plain, frameType)
	plain = append(plain, body...)
	ct := c.sendKey.Seal(nil, nonce, plain, nil)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(ct)))
	if _, err := c.rwc.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.rwc.Write(ct)
	return err
}

// Receive 阻塞读取并解密一帧，返回帧类型与 body。
// 序列号必须严格递增（TCP 本身有序），因此重放/乱序的旧帧会被直接拒绝。
func (c *Conn) Receive() (byte, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.rwc, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	// 最小帧：类型1B + GCM tag 16B
	if n < 17 || n > MaxFrame {
		return 0, nil, fmt.Errorf("%w: 帧长度 %d 越界", ErrBadFrame, n)
	}
	ct := make([]byte, n)
	if _, err := io.ReadFull(c.rwc, ct); err != nil {
		return 0, nil, err
	}
	want := c.recvSeq + 1
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint64(nonce[4:], want)
	plain, err := c.recvKey.Open(nil, nonce, ct, nil)
	if err != nil {
		return 0, nil, ErrAuth
	}
	c.recvSeq = want
	return plain[0], plain[1:], nil
}

func (c *Conn) Close() error { return c.rwc.Close() }

// ---------- FramePacket / FrameMembers 的 body 编解码 ----------

// PacketBody 组装 FramePacket 的 body：src(4B) + dst(4B) + IP 报文。
func PacketBody(src, dst [4]byte, packet []byte) []byte {
	b := make([]byte, 8+len(packet))
	copy(b[0:4], src[:])
	copy(b[4:8], dst[:])
	copy(b[8:], packet)
	return b
}

// ParsePacket 拆解 FramePacket 的 body。payload 与 body 共享底层内存。
func ParsePacket(body []byte) (src, dst [4]byte, payload []byte, err error) {
	if len(body) < 9 {
		err = fmt.Errorf("%w: packet 帧过短", ErrBadFrame)
		return
	}
	copy(src[:], body[0:4])
	copy(dst[:], body[4:8])
	payload = body[8:]
	return
}

func MembersBody(list []MemberInfo) []byte {
	data, _ := json.Marshal(list)
	return data
}

func ParseMembers(body []byte) ([]MemberInfo, error) {
	var list []MemberInfo
	err := json.Unmarshal(body, &list)
	return list, err
}
