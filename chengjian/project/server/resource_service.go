package server

import (
	"context"
	"chengjian.local/chengjian/db"
	"sort"
)

type taskTunnels interface {
	ListByTask(context.Context, int64) ([]*db.TunnelRecord, error)
}
type taskLeases interface {
	ReleaseTask(context.Context, int64) (int64, error)
	Acquire(context.Context, int64, int64, string) error
	OtherHolders(context.Context, int64, int64) ([]int64, error)
}

// ResourceService owns cleanup ordering. Failed/unknown teardown retains leases
// so a dependent session cannot disappear before reconciliation.
type ResourceService struct {
	tunnels  taskTunnels
	leases   taskLeases
	teardown func(context.Context, int64) error
}

func (s *ResourceService) Release(ctx context.Context, taskID int64) TaskResourceRelease {
	out := TaskResourceRelease{TaskID: taskID}
	if taskID <= 0 || s == nil || s.leases == nil {
		return out
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), taskReleaseTimeout)
	defer cancel()

	if s.tunnels != nil && s.teardown != nil {
		recs, err := s.tunnels.ListByTask(ctx, taskID)
		if err != nil {
			out.Failures = append(out.Failures, "读取任务隧道失败: "+err.Error())
		} else {
			sort.Slice(recs, func(i, j int) bool { return recs[i].ID > recs[j].ID })
			for _, rec := range recs {
				if rec.State == "stopped" {
					continue // 已经收过的不再碰，避免把 teardown 的报错当成新失败
				}
				if terr := s.teardown(ctx, rec.ID); terr != nil {
					out.Failures = append(out.Failures, "#"+itoa(rec.ID)+"("+terr.Error()+")")
					continue
				}
				out.Tunnels = append(out.Tunnels, rec.ID)
			}
		}
	}

	if len(out.Failures) > 0 {
		return out
	}

	// 租约最后放：它声明的是「我还在用这些会话」，必须晚于隧道拆除。
	if n, err := s.leases.ReleaseTask(ctx, taskID); err != nil {
		out.Failures = append(out.Failures, "释放会话租约失败: "+err.Error())
	} else {
		out.Leases = n
	}
	return out
}

func (s *ResourceService) Acquire(ctx context.Context, sessionID, taskID int64, purpose string) error {
	if s == nil || s.leases == nil {
		return nil
	}
	return s.leases.Acquire(ctx, sessionID, taskID, purpose)
}
func (s *ResourceService) Holders(ctx context.Context, sessionID, excludeTaskID int64) ([]int64, error) {
	if s == nil || s.leases == nil {
		return nil, nil
	}
	return s.leases.OtherHolders(ctx, sessionID, excludeTaskID)
}
