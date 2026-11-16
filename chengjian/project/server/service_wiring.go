package server

func (s *Server) taskService() *TaskService {
	s.taskServiceOnce.Do(func() {
		var execution taskExecution
		if s.engine != nil {
			execution = s.engine
		}
		s.tasks = &TaskService{ctx: s.ctx, m: s.m, engine: execution, concMu: &s.concMu, bootstrap: s.startTaskEngine, cancelChat: s.cancelTaskChat, resources: s.resourceService()}
	})
	return s.tasks
}
