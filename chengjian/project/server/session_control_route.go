// session_control_route.go 会话控制通道路由（I1 / T-05）。
//
// 问题：第二层 HTTP 会话往往位于只有经隧道才可达的网段。控制请求（probe /
// exec / read / write）若直连，平台根本连不上它——会话登记得再对也用不了，
// 于是「拿到立足点」和「能操作立足点」之间断了一截。
//
// 边界：这里只改**控制通道**的出口，不改目标请求的出口。worker 的目标流量
// 仍由任务代理 + 隧道选路决定（tunnel/route.go），两者互不干扰——把控制通道
// 塞进任务代理会让「探会话」和「打目标」共用一条易变的出口，出问题时分不清
// 是哪一层断了。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/tunnel"
)

// controlRouteUpstream 把一个隧道 id 解析成控制通道可用的代理地址。
//
// 只接受 alive 的 socks 隧道：portfwd 是「固定目标的一条通道」，不能承载任意
// 控制请求（它的本地端口只转发到预先指定的 host:port）。
func (s *Server) controlRouteUpstream(ctx context.Context, tunnelID, sessionID int64) (string, error) {
	if tunnelID <= 0 {
		return "", nil
	}
	if s == nil || s.m == nil || s.m.pg == nil {
		return "", fmt.Errorf("数据库未就绪，无法解析控制通道路由（隧道 #%d）", tunnelID)
	}
	rec, err := db.NewTunnelStore(s.m.pg).Get(ctx, tunnelID)
	if err != nil {
		return "", fmt.Errorf("读取隧道 #%d 失败: %w", tunnelID, err)
	}
	if rec == nil {
		return "", fmt.Errorf("隧道 #%d 不存在", tunnelID)
	}
	if rec.State != db.TunnelAlive {
		return "", fmt.Errorf("隧道 #%d 当前状态是 %s，不是 alive；控制通道经它必然不通，先修隧道再登记会话", tunnelID, rec.State)
	}
	if rec.Kind != tunnel.KindSocks {
		return "", fmt.Errorf("隧道 #%d 是 %s 类型，控制通道只支持 socks（portfwd 只能转发到固定目标）", tunnelID, rec.Kind)
	}
	// 防环：隧道是本会话部署的，控制通道再经它 → 「要连会话得先有隧道，要隧道得
	// 先有会话」。这不是性能问题，是死锁：隧道一断，会话就再也够不着，而重建
	// 隧道又需要这个会话。
	if sessionID > 0 && rec.ViaSessionID == sessionID {
		return "", fmt.Errorf("拒绝成环：隧道 #%d 正是经本会话部署的，控制通道不能再经它", tunnelID)
	}
	var plan tunnel.Plan
	if err := json.Unmarshal(rec.DeployParams, &plan); err != nil || plan.SocksPort <= 0 {
		return "", fmt.Errorf("隧道 #%d 没有可用的本地 socks 端口（部署参数缺失），无法作为控制通道出口", tunnelID)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(plan.SocksPort))
	if rec.Adapter == tunnel.AdapterSuo5 && plan.AuthUser != "" {
		return "socks5://" + plan.AuthUser + ":" + plan.AuthPass + "@" + addr, nil
	}
	return "socks5://" + addr, nil
}
