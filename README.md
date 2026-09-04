# lanparty — An open-source virtual LAN tool for playing games with friends

<p align="left">
  <a href="./LICENSE"><img alt="License: MIT" src="https://img.shields.io/badge/License-MIT-blue.svg"></a>
  <a href="https://go.dev/"><img alt="Go 1.23" src="https://img.shields.io/badge/Go-1.23-00ADD8?logo=go"></a>
  <a href="#已知限制"><img alt="Status: v0.1 experimental" src="https://img.shields.io/badge/status-v0.1%20experimental-orange"></a>
</p>

**lanparty** 是一个开源的「虚拟局域网」联机工具：把分布在不同网络里的几台电脑，通过一台协调节点组成一个虚拟子网，让它们像在同一个局域网里一样互相访问——主打**和朋友联机打游戏**。商业产品中类似的有蒲公英，开源同类项目有 EasyTier。

架构上它由两种角色组成：

- **coord（协调节点）**：部署在一台有公网 IP 的服务器上，负责成员管理、地址分配和流量转发；
- **peer（成员节点）**：每个玩家在本机运行，创建一块虚拟网卡接入虚拟子网。

> **提示：徽章占位。** 上方 License / Go 徽章为占位样式，仓库托管到 GitHub 后可直接使用。

---

## 目录

- [这是什么 / 现在不是什么](#这是什么--现在不是什么)
- [架构](#架构)
- [快速开始](#快速开始)
- [配置文件](#配置文件)
- [工作原理](#工作原理)
- [安全与隐私](#安全与隐私)
- [已知限制](#已知限制)
- [路线图](#路线图)
- [参与开发](#参与开发)
- [License](#license)

---

## 这是什么 / 现在不是什么

**这是什么：**

- 一个把多台机器接入同一个虚拟 IPv4 子网的最小可用工具；
- 端到端加密（AES-256-GCM）的成员间通信；
- 单二进制、零依赖部署（Windows 除外，需 wintun.dll，见下文）。

**现在（v0.1）不是什么 —— 请务必诚实了解当前状态：**

- ⚠️ **v0.1 为实验性版本**，接口和配置格式在后续版本中可能变更；
- ⚠️ **仅中转模式**：所有流量都经过协调节点中转，尚无 P2P 打洞（在路线图中）。协调节点的带宽和质量决定整个虚拟网的体验；
- 虚拟子网**仅支持 IPv4 /24**；
- **不转发广播/组播**报文，依赖 LAN 广播发现的游戏/设备暂时不可用；
- 仅支持 **Windows（需管理员权限 + wintun.dll）** 与 **Linux（需 root）**；**macOS 暂未支持**。

## 架构

```
                      ┌────────────────────────────────┐
                      │        协调节点 coord           │
                      │    虚拟 IP: 10.66.0.1           │
                      │    监听: 0.0.0.0:7800 (TCP)     │
                      │  成员管理 / 地址分配 / 流量转发   │
                      └───────┬─────────┬─────────┬─────┘
                              │         │         │
                    AES-256-GCM 加密帧 (TCP)       │
                              │         │         │
                   ┌──────────┴──┐ ┌────┴─────┐ ┌─┴──────────┐
                   │   玩家 A     │ │  玩家 B   │ │   玩家 C    │
                   │  10.66.0.2  │ │ 10.66.0.3│ │ 10.66.0.4  │
                   │ [虚拟网卡]   │ │ [虚拟网卡] │ │ [虚拟网卡]  │
                   │  lanparty   │ │ lanparty │ │  lanparty  │
                   └─────────────┘ └──────────┘ └────────────┘
```

- 协调节点习惯占用子网第一个地址 `10.66.0.1`，成员从 `.2` 起依次分配；
- 默认子网 `10.66.0.0/24`，默认端口 TCP `7800`；
- MTU 为 `1400`，由协调节点统一下发；
- 成员之间不直接连接，所有包都经协调节点加解密转发。

## 快速开始

### 1. 启动协调节点（一台有公网 IP 的服务器）

```bash
./lanparty coord --network mygame --psk "换成强口令" --listen 0.0.0.0:7800
# 可选: --subnet 10.66.0.0/24   自定义子网
# 可选: --config coord.json     使用配置文件代替命令行参数
```

记得在防火墙/安全组放行 TCP 7800。

### 2. 每个玩家接入（Windows / Linux）

**Windows 前置条件：**

1. 下载 [wintun-0.14.1.zip](https://www.wintun.net/builds/wintun-0.14.1.zip)，把其中的 `wintun/bin/amd64/wintun.dll` 放到 `lanparty.exe` 同一目录；
2. **以管理员身份**运行命令行 / 终端。

**Linux 前置条件：** 需要 root 权限运行（`sudo`）。

```bash
./lanparty peer --coord 服务器IP:7800 --network mygame --psk "换成强口令"
# 可选: --name 我的电脑     在成员列表中显示的名字
# 可选: --iface lanparty   虚拟网卡名称
# 可选: --config peer.json 使用配置文件代替命令行参数
```

### 3. 验证

接入成功后，用 `--name` 报告的名字或协调节点分配的虚拟 IP 互 ping：

```bash
ping 10.66.0.2
ping 10.66.0.3
```

能互相 ping 通虚拟 IP，即组网成功。此时游戏里把房间地址填成对方的虚拟 IP 即可联机。

### 4. 其他命令

```bash
lanparty version   # 查看版本
```

## 配置文件

不想在命令行里敲长参数时，可用 `--config` 指向 JSON 配置文件，示例见 [`examples/`](./examples/)：

- [`examples/coord.example.json`](./examples/coord.example.json) —— 协调节点配置（监听地址、网络名、PSK、子网）；
- [`examples/peer.example.json`](./examples/peer.example.json) —— 成员节点配置（协调节点地址、网络名、PSK、显示名、网卡名）。

复制示例后把 `psk` 换成你自己的强口令即可。命令行参数与配置文件可混用，命令行优先。

## 工作原理

peer 与 coord 之间通过一条 TCP 长连接（默认 `7800` 端口）交换协议帧。帧体使用 AES-256-GCM 加密。

**协议帧类型：**

| 帧类型 | 方向 | 说明 |
| --- | --- | --- |
| `PING` | peer → coord | 心跳保活 |
| `PONG` | coord → peer | 心跳应答 |
| `MEMBERS` | coord → peer | 下发当前成员列表（显示名 + 虚拟 IP） |
| `PACKET` | 双向 | 加密的虚拟网络 IPv4 报文（peer 上行 / coord 下发） |
| `BYE` | peer → coord | 主动退出 |

**握手流程：** peer 发送明文 JSON `Hello`（网络名、显示名等），coord 回复明文 JSON `Welcome`（分配的虚拟 IP、子网、MTU 等）。握手本身不加密，**PSK 的正确性在第一帧解密时才验证**——PSK 错误的 peer 无法解密后续帧，会被立即断开。

**一次转发的完整流程：**

1. 玩家 A 的游戏/系统发出 IP 包 → 本机虚拟网卡；
2. peer 进程从虚拟网卡读取包，用会话密钥 AES-256-GCM 加密，封装为 `PACKET` 帧发给 coord；
3. coord 解密校验后，按目的虚拟 IP 查成员表，重新加密为 `PACKET` 帧发给目标 peer；
4. 玩家 B 的 peer 把包写入本机虚拟网卡 → 系统协议栈 → 游戏。

## 安全与隐私

- **密钥派生：** 所有节点共享同一个 PSK，通过 HKDF-SHA256 派生出双向会话密钥；
- **传输加密：** 帧体使用 AES-256-GCM 认证加密；
- **防重放：** 帧序列号严格递增，拒绝乱序与重复；
- **PSK 验证时机：** 握手（Hello/Welcome）为明文 JSON，PSK 正确性在第一帧解密时验证；
- ⚠️ **诚实声明：** 当前架构下**协调节点能看到全部转发明文流量**（它必须解密才能转发）。请仅与信任的协调节点组网，不要在虚拟网内传输敏感数据。去除这一信任假设的端到端方案在路线图中。

## 已知限制

| 限制 | 说明 |
| --- | --- |
| 仅中转模式 | 所有流量经协调节点，无 P2P 打洞；协调节点带宽 = 全网带宽上限 |
| 仅 IPv4 /24 | 不支持 IPv6，不支持其他掩码长度 |
| 无广播/组播 | 依赖 LAN 广播发现的游戏（如某些局域网搜索大厅）暂不可用，需手动填 IP |
| 平台支持 | Windows 需管理员权限 + wintun.dll；Linux 需 root；**macOS 未支持** |
| 实验性 | v0.1，协议与配置格式可能不兼容变更 |

## 路线图

- [ ] **v0.2：UDP P2P 打洞** + 中继回退（打洞失败自动回落到协调节点中转）
- [ ] 广播 / 组播转发
- [ ] macOS 支持（utun）
- [ ] GUI 客户端
- [ ] 用 Noise 协议替换 PSK（去除协调节点可见明文的信任假设）
- [ ] 子网代理（把虚拟网接到真实子网）

## 参与开发

```bash
# 克隆后直接构建（Go module 名为 lanparty，入口在 ./cmd/lanparty）
go build -trimpath -ldflags "-s -w -X main.version=dev" ./cmd/lanparty

# 运行测试（仓库内置基于内存虚拟网卡的端到端测试，CI 可直接跑）
go test ./...
```

> 中国大陆网络环境下拉取依赖可能超时，请先设置模块代理：
> ```bash
> export GOPROXY="https://goproxy.cn,direct"
> ```

欢迎 Issue 和 PR：报告 bug 请附上平台（Windows/Linux）、命令行与日志；提交代码请确保 `go vet ./...` 与 `go test ./...` 通过。

## License

[MIT](./LICENSE) © 2026 akira
