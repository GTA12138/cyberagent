// planning_counts.go 把「资源有多少」的实时统计喂给意图判据（T-11）。
//
// 为什么单独一个文件：planning 包的判定需要资产/凭据/会话/隧道的计数，而这
// 四类数据分属四个 store，且都要按任务过滤。把它写进 planner 装配处会让那段
// 已经很长的方法再长 60 行；写成独立闭包则每次 graph_overview / add_intent
// 现读一次，状态变化即时反映（缓存反而会让「刚建的隧道不算数」）。
//
// 一条纪律：**任何一类读失败就整份返回零值 Counts（全 nil）**，而不是给部分
// 数据。部分数据会让判定混出「资产 known、隧道 unknown」的半真状态，planner
// 无从判断哪些结论可信。整份 unknown 至少是诚实的。
package server

import (
	"context"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/planning"
)

// planningCountsForTask 返回一个绑定任务的统计闭包，供 Planner/Worker 注入。
func (s *Server) planningCountsForTask(taskID int64) func(context.Context) planning.Counts {
	return func(ctx context.Context) planning.Counts {
		return s.planningCounts(ctx, taskID)
	}
}

// planningCounts 现读本任务的资源计数。
func (s *Server) planningCounts(ctx context.Context, taskID int64) planning.Counts {
	var zero planning.Counts
	if s == nil || s.m == nil || s.m.pg == nil || taskID <= 0 {
		return zero
	}
	pg := s.m.pg

	// 资产：按类型计数。取 5000 条上限——判据只关心「有没有」，不需要精确总量，
	// 但类型分布要真实，所以按页取到上限为止。
	assets, aerr := pg.Assets().QueryByTask(taskID, "", 5000, 0)
	if aerr != nil {
		return zero
	}
	byType := map[string]int{}
	for _, a := range assets {
		if a == nil || a.Type == "" {
			continue
		}
		byType[a.Type]++
	}

	// 凭据：只取 id 与服务标签，不含秘密。
	if s.credStore == nil {
		return zero
	}
	creds, cerr := s.credStore.ListByTask(ctx, taskID)
	if cerr != nil {
		return zero
	}
	credViews := make([]planning.CredentialView, 0, len(creds))
	for _, c := range creds {
		// Service 用凭据来源描述推断出的服务名（与复用打分同一张表），
		// 而不是凭据类型——「有没有 smb 凭据」问的是它能打什么，不是它长什么样。
		svc := ""
		if c != nil {
			svc = db.SourceServiceLabel(c.Source)
		}
		credViews = append(credViews, planning.CredentialView{ID: c.ID, Service: svc, Verified: c.Verified})
	}

	// 会话：只数 alive 且本任务**能用**的（自己建的 + 被授权的）。
	// 只数 CreatedByTask 会漏掉「别的任务建了、授权给我用」的立足点——那正是
	// 前置条件要判断的东西（「我能不能开工」而不是「这活儿是不是我起的」）。
	if s.sessStore == nil {
		return zero
	}
	sessions, serr := s.sessStore.ListAccessible(ctx, taskID, db.SessionAlive)
	if serr != nil {
		return zero
	}
	liveSessions := 0
	for _, sess := range sessions {
		if sess != nil {
			liveSessions++
		}
	}

	// 隧道：只数 alive，并按 kind 分桶（tunnel:socks 判据要用）。
	if s.tunnels == nil {
		return zero
	}
	trecs, terr := s.tunnels.TaskRecords(ctx, taskID)
	if terr != nil {
		return zero
	}
	liveTunnels := 0
	kinds := map[string]int{}
	for _, tr := range trecs {
		if tr == nil || tr.State != db.TunnelAlive {
			continue
		}
		liveTunnels++
		kinds[tr.Kind]++
	}

	return planning.Counts{
		AssetsByType: byType,
		Credentials:  &credViews,
		Sessions:     &liveSessions,
		Tunnels:      &liveTunnels,
		TunnelKinds:  kinds,
	}
}
