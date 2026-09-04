// lanparty 命令行入口。
//
//	lanparty coord  在公网服务器上运行协调节点
//	lanparty peer   在玩家电脑上加入虚拟网络
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"lanparty/internal/coord"
	"lanparty/internal/peer"
	"lanparty/internal/tun"
)

// version 由构建注入：go build -ldflags "-X main.version=v1.0.0"
var version = "dev"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	peer.Version = version

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "coord":
		runCoord(os.Args[2:])
	case "peer":
		runPeer(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("lanparty", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`lanparty —— 开源虚拟局域网联机工具

用法:
  lanparty coord [参数]    在公网服务器上运行协调节点
  lanparty peer  [参数]    在玩家电脑上加入虚拟网络
  lanparty version         显示版本

示例:
  # 公网服务器上:
  lanparty coord --network mygame --psk 换成强口令 --listen 0.0.0.0:7800

  # 每个玩家的电脑上（Windows 需管理员运行）:
  lanparty peer --coord 服务器IP:7800 --network mygame --psk 换成强口令 --name 我的电脑

也可以用 --config 指定 JSON 配置文件，详见 README.md
`)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

func loadJSON(path string, v any) {
	data, err := os.ReadFile(path)
	if err != nil {
		die(err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		die(fmt.Errorf("解析 %s: %w", path, err))
	}
}

func runCoord(args []string) {
	fs := flag.NewFlagSet("coord", flag.ExitOnError)
	cfg := fs.String("config", "", "JSON 配置文件（与命令行参数可混用）")
	listen := fs.String("listen", "0.0.0.0:7800", "监听地址")
	network := fs.String("network", "", "网络名称（简单模式：单网络）")
	psk := fs.String("psk", "", "网络密码（简单模式）")
	subnet := fs.String("subnet", "10.66.0.0/24", "虚拟子网（简单模式，仅支持 /24）")
	fs.Parse(args)

	var opts coord.Options
	if *cfg != "" {
		loadJSON(*cfg, &opts)
	}
	if opts.Listen == "" {
		opts.Listen = *listen
	}
	if *network != "" {
		if opts.Networks == nil {
			opts.Networks = map[string]coord.NetworkConfig{}
		}
		opts.Networks[*network] = coord.NetworkConfig{PSK: *psk, Subnet: *subnet}
	}

	c, err := coord.Parse(opts)
	if err != nil {
		die(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		c.Close()
	}()
	slog.Info("lanparty 协调节点已启动", "listen", c.Addr().String(), "version", version)
	c.Serve()
}

func runPeer(args []string) {
	fs := flag.NewFlagSet("peer", flag.ExitOnError)
	cfg := fs.String("config", "", "JSON 配置文件（与命令行参数可混用）")
	coordAddr := fs.String("coord", "", "协调节点地址 host:port")
	network := fs.String("network", "", "网络名称")
	psk := fs.String("psk", "", "网络密码")
	name := fs.String("name", "", "成员列表里显示的名字（默认主机名）")
	iface := fs.String("iface", "lanparty", "虚拟网卡名")
	fs.Parse(args)

	var opts peer.Options
	if *cfg != "" {
		loadJSON(*cfg, &opts)
	}
	if opts.Coordinator == "" {
		opts.Coordinator = *coordAddr
	}
	if opts.Network == "" {
		opts.Network = *network
	}
	if opts.PSK == "" {
		opts.PSK = *psk
	}
	if opts.Name == "" {
		opts.Name = *name
	}
	if opts.Iface == "" {
		opts.Iface = *iface
	}
	if opts.Coordinator == "" || opts.Network == "" || opts.PSK == "" {
		die(fmt.Errorf("缺少 --coord / --network / --psk 参数，或改用 --config 指定配置文件"))
	}

	factory := func(vip netip.Addr, prefix netip.Prefix, mtu int) (tun.Device, error) {
		return tun.PlatformCreate(opts.Iface, mtu, vip, prefix)
	}
	p := peer.New(opts, factory)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		die(err)
	}
	slog.Info("已退出虚拟网络，再见")
}
