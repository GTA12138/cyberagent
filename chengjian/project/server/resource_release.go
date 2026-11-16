// resource_release.go 任务终态的资源收尾（I4 / T-09 + T-10）。
//
// 问题：任务停止、删除、归档之后，它布下的隧道与会话租约仍然存活。隧道的
// 「重拉」需要会话，会话的「存活」由任务持有——于是三者构成一条链，任务一死，
// 链上资源既不能被任何入口操作，也不会自己消失。会话删除路径在轮 3 已按依赖
// 逆序收尾（server/intranet.go），但任务终态路径没有，这里补上。
//
// 顺序不能反：**先隧道，后租约**。隧道依赖会话（部署要经立足点），租约是
// 「这个任务还在用会话」的存活声明。先放租约等于先宣布「我不再用它了」，
// 再去拆建在它上面的东西——中间窗口里别人可以合法地把会话删掉，于是隧道
// 拆不掉、记录悬空。
package server

import (
	"context"
	"fmt"
	"log"
	"time"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/db"
)

// taskReleaseTimeout 是终态收尾的墙钟上限。
//
// 收尾不能挂在请求的 context 上：客户端断开、或删除请求已返回 202 之后，
// 请求 ctx 会被取消，而远端进程还在。用独立 ctx 让收尾跑完，是「宁可多花
// 几十秒，也不要留下一个查不到的悬空隧道」的具体实现。
const taskReleaseTimeout = 90 * time.Second

// TaskResourceRelease 是一次终态收尾的结果，用于审计与响应体。
type TaskResourceRelease struct {
	TaskID   int64    `json:"task_id"`
	Tunnels  []int64  `json:"tunnels_torn_down,omitempty"`
	Leases   int64    `json:"leases_released"`
	Failures []string `json:"failures,omitempty"`
}

// Empty 表示这次收尾什么都没做（没有隧道也没有租约），调用方据此决定是否
// 往响应里塞这个字段——空结构体不该污染正常响应。
func (r TaskResourceRelease) Empty() bool {
	return len(r.Tunnels) == 0 && r.Leases == 0 && len(r.Failures) == 0
}

// noteSessionLease 记录「本任务正在使用这个会话」。
//
// best-effort：写不进去只记日志。理由是执行本身已经发生——因为一条簿记记录
// 失败而拒绝执行，等于让 agent 白跑一轮，代价高于「少一条租约」。代价是删会话
// 时的保护会弱一档，所以这里必须留日志，让「为什么没拦住」可追。
func (s *Server) noteSessionLease(ctx context.Context, sessionID int64, purpose string) {
	if s == nil || s.m == nil || s.m.pg == nil || sessionID <= 0 {
		return
	}
	ri := agent.RunInfoFrom(ctx)
	if ri.TaskID <= 0 {
		return
	}
	if err := s.resourceService().Acquire(ctx, sessionID, ri.TaskID, purpose); err != nil {
		log.Printf("[lease] 会话 #%d 租约登记失败（任务 %d，不影响本次执行）: %v", sessionID, ri.TaskID, err)
	}
}

// sessionLeaseHolders 返回除 excludeTaskID 外仍持有该会话的任务。
//
// 读失败返回 error 而不是空切片：调用方据此决定「拒绝删除」，而不是在不确定
// 的情况下放行——放行的代价是掐断别人的执行。
func (s *Server) sessionLeaseHolders(ctx context.Context, sessionID, excludeTaskID int64) ([]int64, error) {
	if s == nil || s.m == nil || s.m.pg == nil || sessionID <= 0 {
		return nil, nil
	}
	return s.resourceService().Holders(ctx, sessionID, excludeTaskID)
}

// releaseTaskResources 按依赖逆序释放任务持有的受管资源。
//
// 隧道按 id 倒序拆：隧道的依赖关系没有显式图（会话由 agent 登记，不记录
// 「这个会话是经哪条隧道到达的」），所以用创建顺序的逆序作为近似——后建的
// 隧道更可能依赖先建的（先打通 socks，再在其上布 portfwd）。这是近似而非
// 保证，因此失败项只记录、不重试，留给人工或健康巡检。
func (s *Server) releaseTaskResources(taskID int64) TaskResourceRelease {
	return s.resourceService().Release(context.Background(), taskID)
}

func (s *Server) resourceService() *ResourceService {
	service := &ResourceService{}
	if s == nil || s.m == nil || s.m.pg == nil {
		return service
	}
	service.leases = db.NewSessionLeaseStore(s.m.pg)
	service.tunnels = db.NewTunnelStore(s.m.pg)
	service.teardown = s.teardownTunnelResource
	return service
}

// teardownTunnelResource 执行单条隧道的收尾。
//
// 抽成一层是为了让「收尾顺序与失败保留」这条语义可以被端到端验证：真实隧道
// 需要真实远端，而这条语义的关键（倒序、跳过已收、失败保留并报告）与远端
// 无关。测试注入 override 后，走的仍是 releaseTaskResources 的真实代码路径。
func (s *Server) teardownTunnelResource(ctx context.Context, id int64) error {
	if s.tunnelTeardownOverride != nil {
		return s.tunnelTeardownOverride(ctx, id)
	}
	if s.tunnels == nil {
		return fmt.Errorf("隧道子系统未就绪")
	}
	return s.tunnels.Teardown(ctx, id)
}

// stopTaskAndRelease 是「停止任务 + 收尾资源」的唯一入口，供删除/归档路径调用。
//
// 收尾失败不改变删除/归档已经成功的结论——那时 PostgreSQL 已经提交，回滚
// 是不可能的。失败项以返回值交给调用方写进响应/审计，让「有隧道没拆掉」
// 这件事可见，而不是静默吞掉。
func (s *Server) stopTaskAndRelease(taskID string) TaskResourceRelease {
	if s == nil {
		return TaskResourceRelease{}
	}
	rel := s.taskService().Stop(context.Background(), taskID)
	if len(rel.Failures) > 0 {
		log.Printf("[release] 任务 %s 资源收尾未完成: %v", taskID, rel.Failures)
	}
	return rel
}

// itoa 避免为一个日志片段引入 strconv 依赖冲突（本文件已有手写解析）。
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

// joinInt64s 把任务 id 列表渲染成 "#1, #2" 形式，用于 409 响应正文——
// 让人一眼看出「是谁在用」，而不是只得到一个「被占用」。
func joinInt64s(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ", "
		}
		out += "#" + itoa(id)
	}
	return out
}
