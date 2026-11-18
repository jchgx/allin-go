// Package netfix 处理 Android 上特殊的网络环境。
//
// 问题：Android 没有 /etc/resolv.conf，Go 的 pure-Go 解析器会直接失败，
// 表现是「curl 能通、Go 程序全部 network error」。
//
// 解决：自己装一个 Resolver，按 环境变量 → net.dns1 → net.dns2 → 公共 DNS 的顺序
// 挑可用的服务器。只影响本进程，不改系统任何东西。
package netfix

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var (
	once    sync.Once
	servers []string
)

// Setup 安装带兜底的解析器，并返回实际使用的 DNS 列表。
func Setup() []string {
	once.Do(func() {
		servers = detectServers()
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				dialer := net.Dialer{Timeout: 4 * time.Second}
				var lastErr error
				for _, srv := range servers {
					conn, err := dialer.DialContext(ctx, "udp", srv)
					if err == nil {
						return conn, nil
					}
					lastErr = err
				}
				if lastErr == nil {
					lastErr = &net.DNSError{Err: "没有可用的 DNS 服务器", IsNotFound: false}
				}
				return nil, lastErr
			},
		}
	})
	return servers
}

// detectServers 按优先级拼候选列表。
func detectServers() []string {
	var out []string
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		if !strings.Contains(host, ":") {
			host = net.JoinHostPort(host, "53")
		}
		for _, x := range out {
			if x == host {
				return
			}
		}
		out = append(out, host)
	}

	// 1) 显式指定
	for _, h := range strings.Split(os.Getenv("ALLIN_DNS"), ",") {
		add(h)
	}
	// 2) Android 系统属性
	for _, prop := range []string{"net.dns1", "net.dns2", "net.dns3", "net.dns4"} {
		if v := getprop(prop); v != "" && v != "0.0.0.0" {
			add(v)
		}
	}
	// 3) 公共 DNS 兜底（国内可达优先）
	//
	// 不再放 8.8.8.8：国内多数网络到它没有路由，加了它只会让最后的
	// 报错变成「dial udp 8.8.8.8:53: connect: network is unreachable」，
	// 把「DNS 服务器不可达」写成一句看起来像本机网卡坏了的话
	// （线上日志里就是这么误导排查的）。真要海外 DNS，用 ALLIN_DNS 指定。
	add("223.5.5.5")
	add("119.29.29.29")

	return out
}

// getprop 读一个 Android 系统属性，失败返回空串。
func getprop(name string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "getprop", name).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
