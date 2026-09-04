# lanparty Linux 服务器部署套件（deploy/）

本目录提供协调节点（`lanparty coord`）的一键部署、systemd 单元模板与真机自测脚本，仅在 Linux 服务器上使用。所有脚本都只做部署/测试，不会改动 lanparty 本体。

| 文件 | 用途 |
| --- | --- |
| `install-server.sh` | 一键把 coord 安装为 systemd 服务（`lanparty-coord`） |
| `lanparty-coord.service` | systemd 单元模板，供手工部署参考 |
| `selftest-server.sh` | 真机端到端自测：1 coord + 2 peer（同机双 TUN 互 ping） |

背景速览：

- coord 默认监听 TCP 7800；虚拟子网 10.66.0.0/24，成员从 10.66.0.2 开始分配。
- coord 普通用户即可运行；peer 在 Linux 上创建 TUN 需要 root。
- peer 通过 `--iface` 指定 TUN 网卡名（Linux 网卡名最长 15 字符）。
- peer 断线自动重连；coord 不做 PSK 预验证，PSK 错误的 peer 在首帧解密失败时被断开（可在日志看到）。

## 1. 一键安装协调节点

把二进制和本目录拷到服务器后：

```bash
sudo ./install-server.sh <lanparty二进制路径> <网络名> <PSK> [监听地址，默认 0.0.0.0:7800]

# 示例：
sudo ./install-server.sh ./lanparty party-lan "$(openssl rand -hex 16)"
sudo ./install-server.sh ./lanparty party-lan 'my-psk' 0.0.0.0:7800
```

脚本行为：安装二进制到 `/usr/local/bin/lanparty` → 生成 `/etc/systemd/system/lanparty-coord.service`（`Restart=always`、`RestartSec=5`、`After=network.target`、`WantedBy=multi-user.target`，用专用系统账号 `lanparty` 运行）→ `systemctl daemon-reload && systemctl enable --now` → `systemctl status --no-pager` 显示结果并提示放行端口。重复执行即可升级二进制或修改参数（脚本会先停旧服务再覆盖）。

常用运维命令：

```bash
systemctl status lanparty-coord
journalctl -u lanparty-coord -f
# 卸载服务：
sudo systemctl disable --now lanparty-coord
sudo rm /etc/systemd/system/lanparty-coord.service
```

## 2. 真机自测

在一台有 `/dev/net/tun` 的 Linux 服务器上（需要 root，依赖 openssl/ip/ping）：

```bash
sudo ./selftest-server.sh                 # 默认使用当前目录 ./lanparty
sudo ./selftest-server.sh /opt/lanparty   # 显式指定二进制路径
```

脚本流程：打印系统信息（uname / nproc / free / /dev/net/tun）→ 随机生成网络名与 PSK、选空闲端口（默认 17800，被占则 +1）→ 后台启动 coord → 启动两个 peer（`--iface lanparty-t0` 与 `lanparty-t1`，注意网卡名仅 11 字符）→ 等两个 10.66.0.x 虚拟 IP 就绪（超时 20 秒）→ 添加 /32 主机路由消除双 TUN 路由歧义 → `ping -c 5 -W 2` 小包与 `ping -c 3 -s 1200` 大包互测 → 汇总 PASS/FAIL 与耗时。

预期输出（节选）：

```text
[selftest] /dev/net/tun: 存在
[selftest] coord 已就绪（PID=12345）
[selftest] lanparty-t0 虚拟 IP = 10.66.0.2；lanparty-t1 虚拟 IP = 10.66.0.3
5 packets transmitted, 5 received, 0% packet loss, time 4011ms
rtt min/avg/max/mdev = 0.4/0.6/0.9/0.1 ms
==========================================================
 lanparty 真机自测结果: PASS（耗时 16s）
   lanparty-t0 VIP : 10.66.0.2
   lanparty-t1 VIP : 10.66.0.3
   ping 小包 (-c 5 -W 2)    : PASS
   ping 大包 (-c 3 -s 1200) : PASS
==========================================================
```

说明：

- 自测使用随机网络名/PSK 并只监听 `127.0.0.1`，与生产服务（默认 0.0.0.0:7800）互不冲突。
- 脚本会临时放宽 `net.ipv4.conf.rp_filter`（同机双 TUN 回包路径需要），退出时自动恢复原值。
- 任何退出路径（成功/失败/Ctrl-C）都会清理全部进程与 `lanparty-t0/t1` 网卡，不安装持久服务、无残留。
- 任一步失败会输出 FAIL、coord/peer 日志末尾与 `ip addr/route` 现场，可直接贴日志排查。

## 3. 手工 systemd 部署

不想跑脚本时，参考 `lanparty-coord.service` 模板：把 `<NETWORK>` / `<PSK>` / `<LISTEN>` 三个占位符替换为实际值，拷贝到 `/etc/systemd/system/lanparty-coord.service`，然后：

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin lanparty
sudo systemctl daemon-reload
sudo systemctl enable --now lanparty-coord
sudo systemctl status lanparty-coord --no-pager
```

## 4. 防火墙 / 云安全组提醒

成员节点需要通过 TCP 连接 coord 的监听端口（默认 7800），部署后务必放行：

```bash
sudo ufw allow 7800/tcp
# 或 firewalld：
sudo firewall-cmd --permanent --add-port=7800/tcp && sudo firewall-cmd --reload
# 或 iptables：
sudo iptables -I INPUT -p tcp --dport 7800 -j ACCEPT
```

云服务器（阿里云/腾讯云/AWS 等）还需在控制台安全组放行对应 TCP 端口。请同时确认各 peer 主机的出站访问没有被网络策略拦截。
