package server

import (
	"context"
	"fmt"

	"chengjian.local/chengjian/db"
)

func (s *Server) authorizeTunnelRedeploy(ctx context.Context, rec *db.TunnelRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if rec == nil || rec.TaskID <= 0 || rec.ViaSessionID <= 0 || s.sessStore == nil {
		return fmt.Errorf("重部署所需的任务、会话或授权存储不可用")
	}
	allowed, err := s.sessStore.CanAccess(ctx, rec.ViaSessionID, rec.TaskID, db.SessionActionTunnelDeploy)
	if err != nil {
		return fmt.Errorf("重部署会话授权读取失败: %w", err)
	}
	if !allowed {
		return fmt.Errorf("任务 %d 已无会话 %d 的 tunnel_deploy 授权", rec.TaskID, rec.ViaSessionID)
	}
	return nil
}
