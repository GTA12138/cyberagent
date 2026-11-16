package server

import "context"

// These tests exercise HTTP CRUD contracts, not the task execution engine.
// A canceled service context keeps startup restoration, scheduling and external
// model calls out of API tests while each HTTP request retains its own context.
// Execution tests must supply their own live, cancellable context instead.
func apiOnlyContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// Execution fixtures need a live context and must stop their loops before
// their deferred Manager.Close and TempDir cleanup release dependent stores.
func drainTestServer(s *Server, cancel context.CancelFunc) {
	cancel()
	s.engine.runtimeMu.Lock()
	ids := make([]string, 0, len(s.engine.runtimes))
	for id := range s.engine.runtimes {
		ids = append(ids, id)
	}
	s.engine.runtimeMu.Unlock()
	for _, id := range ids {
		s.engine.StopTask(id)
	}
	s.archiveWG.Wait()
}
