package server

import (
	"context"
	"fmt"
	"strconv"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/traffic"
	actool "github.com/Autumn-27/norma/tool"
)

func defaultTrafficSource(taskID int64) string {
	if taskID > 0 {
		return "task/" + strconv.FormatInt(taskID, 10)
	}
	return "global"
}

func checkTrafficSource(taskID int64, source string) error {
	if err := traffic.ValidateSource(source); err != nil {
		return err
	}
	if source != "global" && source != defaultTrafficSource(taskID) {
		return fmt.Errorf("流量源不属于当前任务：%s", source)
	}
	return nil
}

func (m *Manager) trafficSource(taskID int64, requested string) (traffic.LookupSource, error) {
	if requested == "" {
		requested = defaultTrafficSource(taskID)
	}
	if err := checkTrafficSource(taskID, requested); err != nil {
		return traffic.LookupSource{}, err
	}
	return traffic.LookupSource{Name: requested, With: func(ctx context.Context, fn func(*traffic.Traffic) error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if requested == "global" {
			if m.traffic == nil {
				return fmt.Errorf("全局流量历史不可用")
			}
			return fn(m.traffic)
		}
		if m.taskProxy == nil {
			return fmt.Errorf("任务流量历史不可用")
		}
		return m.taskProxy.withTraffic(ctx, taskID, fn)
	}}, nil
}

func (m *Manager) scopedTrafficTools(assemblyCtx context.Context) []actool.CoreTool {
	if m.traffic == nil || !m.TrafficEnabled() {
		return nil
	}
	assembled := agent.RunInfoFrom(assemblyCtx)
	return traffic.ScopedTools(func(ctx context.Context, requested string) (traffic.LookupSource, error) {
		ri, explicit := agent.LookupRunInfo(ctx)
		if !explicit {
			ri = assembled
		}
		if requested == "" {
			requested = ri.TrafficSource
			if requested == "" {
				requested = "global" // planner/chat and legacy callers use global egress
			}
		}
		return m.trafficSource(ri.TaskID, requested)
	})
}

func (m *Manager) readTrafficEvidence(ctx context.Context, taskID int64, id string, consume func(traffic.EvidenceExchange) error) error {
	// Legacy UI/API IDs have always referred to the global store. Preserve that
	// meaning even when the finding has a task; new task IDs are always qualified.
	source, raw, err := traffic.ParseReference(id, "global")
	if err != nil {
		return err
	}
	lookup, err := m.trafficSource(taskID, source)
	if err != nil {
		return err
	}
	return lookup.With(ctx, func(tr *traffic.Traffic) error {
		return tr.ReadEvidence(ctx, []string{raw}, func(e traffic.EvidenceExchange) error {
			e.ID = source + "/" + e.ID
			return consume(e)
		})
	})
}
