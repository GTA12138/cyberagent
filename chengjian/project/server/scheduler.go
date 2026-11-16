package server

import (
	"context"
	"fmt"
	"chengjian.local/chengjian/db"
	"log"
	"strings"
	"time"
)

// Business events and recipients are captured by database triggers in the write
// transaction. This scheduler only creates durable interval slots and admits tasks.
type Scheduler struct {
	s    *Server
	pg   *db.DB
	tick time.Duration
}

func newScheduler(s *Server) *Scheduler { return &Scheduler{s: s, pg: s.m.pg, tick: 5 * time.Second} }
func (sc *Scheduler) Run(ctx context.Context) {
	if sc.pg == nil {
		return
	}
	sc.step()
	ticker := time.NewTicker(sc.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sc.s.reconcileConcurrency()
			sc.step()
		}
	}
}
func (sc *Scheduler) step() {
	ctx, cancel := context.WithTimeout(sc.s.ctx, 5*time.Second)
	defer cancel()
	if err := sc.pg.EnqueueDueTriggerIntervals(ctx); err != nil {
		log.Printf("[trigger] interval outbox: %v", err)
	}
}

// containsFold reports whether name is in set (case-insensitive).

func containsFold(set []string, name string) bool {
	for _, s := range set {
		if strings.EqualFold(s, name) {
			return true
		}
	}
	return false
}

// trunc caps s to max runes, appending an ellipsis + original length when cut.
func trunc(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("…(已截断,共 %d 字)", len(r))
}
