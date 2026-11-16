package server

import (
	"context"
	"fmt"

	"chengjian.local/chengjian/agent"
	"chengjian.local/chengjian/session"
)

func (s *Server) requireResourceTask(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	taskID := agent.RunInfoFrom(ctx).TaskID
	if taskID <= 0 {
		return fmt.Errorf("资源操作必须在明确的任务上下文内使用")
	}
	active, err := s.sessStore.TaskActive(ctx, taskID)
	if err != nil {
		return fmt.Errorf("任务授权读取失败，操作已拒绝: %w", err)
	}
	if !active {
		return fmt.Errorf("任务不存在、已删除或已归档，资源操作已拒绝")
	}
	return nil
}

// lookupSession is the agent boundary. HTTP management and system cleanup use
// their own authenticated/internal entry points; TaskID zero grants no privilege.
func (s *Server) lookupSession(ctx context.Context, id int64, action string) (session.Session, error) {
	if id <= 0 {
		return nil, fmt.Errorf("session_id 必填且 > 0")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	taskID := agent.RunInfoFrom(ctx).TaskID
	if taskID <= 0 {
		return nil, fmt.Errorf("会话操作必须在明确的任务上下文内使用")
	}
	allowed, err := s.sessStore.CanAccess(ctx, id, taskID, action)
	if err != nil {
		return nil, fmt.Errorf("会话授权读取失败，操作已拒绝: %w", err)
	}
	if !allowed {
		return nil, fmt.Errorf("任务 %d 未获会话 %d 的 %s 操作授权", taskID, id, action)
	}
	if s.sessReg == nil {
		return nil, fmt.Errorf("会话注册表未就绪")
	}
	sess, ok := s.sessReg.Get(id)
	if !ok {
		return nil, fmt.Errorf("session %d 不在注册表（未登记/已关闭/恢复失败）", id)
	}
	return sess, nil
}

// Ownership is checked before lifecycle operations: a rejected stop must not
// cancel another task's in-flight deployment via beginOperation.
func (s *Server) authorizeTunnel(ctx context.Context, id int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	taskID := agent.RunInfoFrom(ctx).TaskID
	if taskID <= 0 {
		return fmt.Errorf("隧道操作必须在明确的任务上下文内使用")
	}
	if s.tunnels == nil {
		return fmt.Errorf("隧道子系统未就绪")
	}
	rec, err := s.tunnels.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("隧道归属读取失败，操作已拒绝: %w", err)
	}
	if rec == nil || rec.TaskID != taskID {
		return fmt.Errorf("任务 %d 无权操作隧道 %d", taskID, id)
	}
	return nil
}
