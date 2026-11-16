// lateral_route.go 横向执行的路由与投递层。
//
// 解决的问题：impacket 五件套（psexec/wmiexec/smbexec/atexec/dcomexec）没有
// 原生 socks 参数（fortra/impacket 至今没有，issue #645 的官方答案就是
// proxychains），此前 lateral_exec 的 impacket 分支在平台本地直接起 python 进程
// ——目标只在隧道另一侧可达时，SMB 流量从平台出口直连，全部报 no route，
// 而失败签名又会被归因为 ReasonNetwork「检查隧道可达性」：隧道明明是通的。
// Windows 内网横向（内网渗透的主要工作量）与隧道子系统事实上没有打通。
//
// 方案：按目标的任务选路结论（RouteForTaskHost）决定执行形态——
//   - tunnel：经 proxychains-ng 把 impacket 的全部 TCP connect（含 DCOM 动态
//     端口与 DNS）挂到任务隧道的 socks 入口上。选 proxychains 而不是自写
//     socks 参数注入，是因为 DCOM 的动态端口无法逐端口预映射，「钩住
//     connect()」是唯一能覆盖全部出站的通用做法；
//   - blocked：内网期无存活隧道，直接拒绝（不直连）；
//   - global：外网阶段，直连是合法形态，维持既有行为。
//
// proxychains 缺失不是静默降级的理由：降级=从平台直连内网目标，正是要阻止
// 的行为。报错里给安装指引与替代路径（session_exec 经立足点执行）。
package server

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// proxychainsCandidates 是 PATH 查找顺序（ng 版优先：支持 socks5 认证与
// remote DNS；老 proxychains 只到 socks4）。
var proxychainsCandidates = []string{"proxychains4", "proxychains"}

// locateProxychains 定位 proxychains 可执行文件。
//
// 顺序：CHENGJIAN_PROXYCHAINS_PATH > data/tools/proxychains{,4} > PATH。
// 与 chisel/suo5 的解析约定一致（CHENGJIAN_*_PATH 覆盖 > data/tools 兜底）。
func locateProxychains(dataDir string) (string, error) {
	if p := strings.TrimSpace(os.Getenv("CHENGJIAN_PROXYCHAINS_PATH")); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		return "", fmt.Errorf("CHENGJIAN_PROXYCHAINS_PATH=%q 不是可执行文件", p)
	}
	for _, name := range []string{"proxychains", "proxychains4"} {
		p := filepath.Join(dataDir, "tools", name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	for _, name := range proxychainsCandidates {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 proxychains-ng（横向目标只经隧道可达时，impacket 的 SMB/DCOM 流量必须经 socks 转发，" +
		"而 impacket 没有原生代理参数）。安装 proxychains4（Debian/Kali: apt install proxychains4; " +
		"源码: github.com/rofl0r/proxychains-ng，编译后放 data/tools/ 或设 CHENGJIAN_PROXYCHAINS_PATH），" +
		"或改用 session_exec 经 Linux 立足点执行、lateral_exec method=ssh 打 Linux 目标")
}

// writeProxychainsConf 生成一次性的 proxychains-ng 配置。
//
// 只写这一条链：strict_chain（上游挂了就失败，不静默直连——这是选它的核心理由，
// quiet_mode 关掉也无妨，-q 已足够）+ proxy_dns（内网域名经 socks 在目标侧解析，
// 平台解析不到内网域）。
//
// ProxyList 行格式（proxychains-ng 官方契约）：`socks5 IP PORT [USER PASS]`——
// **IP 与端口是分开的字段**，不是 host:port 合写。旧实现写 `socks5 127.0.0.1:20801`，
// proxychains 解析端口失败、整条链不生效，这是外部评审确认的配置格式错误。
// 返回配置路径与清理函数；调用方必须执行清理（0600 文件里有 socks 口令）。
func writeProxychainsConf(upstream string) (string, func(), error) {
	// parseSocksURL 不校验 scheme（它只剥前缀），先在这里挡掉非 socks 上游——
	// 任务上游理论上只有 socks5://（pickUpstream/upstreamOf 构造），但拦一道
	// 比生成一份指向 http 代理的 socks5 配置好。
	if !strings.HasPrefix(upstream, "socks5://") && !strings.HasPrefix(upstream, "socks5h://") {
		return "", nil, fmt.Errorf("上游 %q 不是 socks5:// 形式，无法生成 proxychains 配置", upstream)
	}
	u, err := parseSocksURL(upstream)
	if err != nil {
		return "", nil, fmt.Errorf("任务隧道的 socks 上游 %q 无法解析: %w", upstream, err)
	}
	host, port, err := net.SplitHostPort(u.addr)
	if err != nil {
		return "", nil, fmt.Errorf("socks 入口 %q 缺少 host:port: %w", u.addr, err)
	}
	var b strings.Builder
	b.WriteString("strict_chain\n")
	b.WriteString("proxy_dns\n")
	b.WriteString("tcp_read_time_out 15000\ntcp_connect_time_out 8000\n")
	b.WriteString("[ProxyList]\n")
	if u.user != "" {
		fmt.Fprintf(&b, "socks5 %s %s %s %s\n", host, port, u.user, u.pass)
	} else {
		fmt.Fprintf(&b, "socks5 %s %s\n", host, port)
	}
	f, err := os.CreateTemp("", "chengjian-pc-*.conf")
	if err != nil {
		return "", nil, err
	}
	path := f.Name()
	cleanup := func() { _ = os.Remove(path) }
	if _, err := f.WriteString(b.String()); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
