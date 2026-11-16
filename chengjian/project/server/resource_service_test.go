package server

import (
	"context"
	"errors"
	"chengjian.local/chengjian/db"
	"testing"
)

type resourceTestLeases struct{ released bool }

func (s *resourceTestLeases) ReleaseTask(ctx context.Context, _ int64) (int64, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	s.released = true
	return 1, nil
}
func (s *resourceTestLeases) Acquire(context.Context, int64, int64, string) error { return nil }
func (s *resourceTestLeases) OtherHolders(context.Context, int64, int64) ([]int64, error) {
	return nil, nil
}

type resourceTestTunnels struct{}

func (resourceTestTunnels) ListByTask(context.Context, int64) ([]*db.TunnelRecord, error) {
	return []*db.TunnelRecord{{ID: 1}, {ID: 2}, {ID: 3, State: "stopped"}}, nil
}

func TestResourceServiceRetainsLeasesUntilConfirmed(t *testing.T) {
	leases := &resourceTestLeases{}
	fail := true
	order := []int64{}
	service := &ResourceService{leases: leases, tunnels: resourceTestTunnels{}, teardown: func(ctx context.Context, id int64) error {
		if ctx.Err() != nil {
			t.Fatal("request cancellation interrupted cleanup")
		}
		if leases.released {
			t.Fatal("lease released before tunnel")
		}
		order = append(order, id)
		if fail && id == 2 {
			return errors.New("unknown teardown")
		}
		return nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	first := service.Release(ctx, 1)
	if leases.released || len(first.Failures) != 1 || len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("incorrect cleanup: %+v order=%v", first, order)
	}
	fail = false
	second := service.Release(ctx, 1)
	if !leases.released || second.Leases != 1 || len(second.Failures) != 0 {
		t.Fatalf("retry did not finish cleanup: %+v", second)
	}
}
