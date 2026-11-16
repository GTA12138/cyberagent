package server

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type dashboardPoint struct {
	At     time.Time `json:"at"`
	CPU    *float64  `json:"cpu"`
	Memory *float64  `json:"memory"`
	RX     *float64  `json:"rx"`
	TX     *float64  `json:"tx"`
}
type dashboardDisk struct {
	Mount   string  `json:"mount"`
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	Percent float64 `json:"percent"`
}
type dashboardProcess struct {
	PID  int32  `json:"pid"`
	Name string `json:"name"`
	RSS  uint64 `json:"rss"`
}
type dashboardSystemData struct {
	SampledAt   time.Time          `json:"sampled_at"`
	Hostname    string             `json:"hostname"`
	Platform    string             `json:"platform"`
	CPUModel    string             `json:"cpu_model"`
	Threads     int                `json:"threads"`
	Uptime      uint64             `json:"uptime"`
	Load        *load.AvgStat      `json:"load"`
	MemoryTotal uint64             `json:"memory_total"`
	MemoryUsed  uint64             `json:"memory_used"`
	Cores       []float64          `json:"cores"`
	Disks       []dashboardDisk    `json:"disks"`
	DisksAt     time.Time          `json:"disks_at"`
	Processes   []dashboardProcess `json:"processes"`
	ProcessesAt time.Time          `json:"processes_at"`
	History     []dashboardPoint   `json:"history"`
	Unavailable []string           `json:"unavailable"`
}

// One serialized sampler per Server. Clients share real samples; more browser
// tabs cannot shorten CPU/network measurement intervals or multiply process scans.
type dashboardTelemetry struct {
	mu          sync.Mutex
	data        dashboardSystemData
	network     map[string]gnet.IOCountersStat
	networkAt   time.Time
	diskPending chan dashboardDiskResult
	diskPartial bool
}

type dashboardDiskResult struct {
	disks   []dashboardDisk
	at      time.Time
	partial bool
}

// Windows disk APIs can ignore context for an unavailable network drive. Keep
// at most one disk probe in flight, separate from the fast telemetry sampler.
func collectDashboardDisks(result chan<- dashboardDiskResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	partitions, err := disk.PartitionsWithContext(ctx, false)
	out := dashboardDiskResult{disks: []dashboardDisk{}, at: time.Now().UTC(), partial: err != nil}
	seen := map[string]bool{}
	for _, part := range partitions {
		if ctx.Err() != nil {
			out.partial = true
			break
		}
		if seen[part.Mountpoint] {
			continue
		}
		seen[part.Mountpoint] = true
		if u, err := disk.UsageWithContext(ctx, part.Mountpoint); err == nil && u.Total > 0 {
			out.disks = append(out.disks, dashboardDisk{part.Mountpoint, u.Total, u.Used, u.UsedPercent})
		} else {
			out.partial = true
		}
	}
	result <- out
}

func networkRate(previous, current uint64, seconds float64) float64 {
	if seconds <= 0 || current < previous {
		return 0
	}
	return float64(current-previous) / seconds
}

func (s *Server) dashboardSystem(w http.ResponseWriter, r *http.Request) {
	t := &s.dashboardTelemetry
	t.mu.Lock()
	w.Header().Set("Cache-Control", "no-store")
	if time.Since(t.data.SampledAt) < 2*time.Second {
		data := t.data
		t.mu.Unlock()
		writeJSON(w, 200, data)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	d := t.data
	if d.Disks == nil {
		d.Disks = []dashboardDisk{}
	}
	d.Unavailable = []string{}
	if d.Hostname == "" {
		if h, err := host.InfoWithContext(ctx); err == nil {
			d.Hostname, d.Platform = h.Hostname, h.Platform+" "+h.PlatformVersion+" / "+runtime.GOARCH
		}
		if info, err := cpu.InfoWithContext(ctx); err == nil && len(info) > 0 {
			d.CPUModel = info[0].ModelName
		}
		d.Threads = runtime.NumCPU()
	}
	if uptime, err := host.UptimeWithContext(ctx); err == nil {
		d.Uptime = uptime
	}
	// Windows exposes no native Unix load average. gopsutil's emulation owns
	// a process-wide background sampler; a request context would freeze it.
	if runtime.GOOS != "windows" {
		d.Load, _ = load.AvgWithContext(ctx)
	}
	if d.Load == nil {
		d.Unavailable = append(d.Unavailable, "load")
	}
	now := time.Now().UTC()
	p := dashboardPoint{At: now}
	cores, err := cpu.PercentWithContext(ctx, 0, true)
	if err == nil && len(cores) > 0 && !d.SampledAt.IsZero() {
		d.Cores = cores
		avg := 0.0
		for _, v := range cores {
			avg += v
		}
		avg /= float64(len(cores))
		p.CPU = &avg
	} else {
		d.Cores = []float64{}
		d.Unavailable = append(d.Unavailable, "cpu")
	}
	if m, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		d.MemoryTotal, d.MemoryUsed = m.Total, m.Used
		p.Memory = &m.UsedPercent
	} else {
		d.MemoryTotal, d.MemoryUsed = 0, 0
		d.Unavailable = append(d.Unavailable, "memory")
	}
	interfaces, interfaceErr := gnet.InterfacesWithContext(ctx)
	if counters, err := gnet.IOCountersWithContext(ctx, true); err == nil && interfaceErr == nil {
		// Omit loopback and adapters without a live link. Track each adapter
		// separately so newly appearing/reset counters never create rate spikes.
		active := map[string]bool{}
		for _, iface := range interfaces {
			up, loopback := false, false
			for _, flag := range iface.Flags {
				if flag == "up" {
					up = true
				}
				if flag == "loopback" {
					loopback = true
				}
			}
			active[iface.Name] = up && !loopback
		}
		next := map[string]gnet.IOCountersStat{}
		rx, tx := 0.0, 0.0
		for _, c := range counters {
			if !active[c.Name] {
				continue
			}
			next[c.Name] = c
			if old, ok := t.network[c.Name]; ok {
				rx += networkRate(old.BytesRecv, c.BytesRecv, now.Sub(t.networkAt).Seconds())
				tx += networkRate(old.BytesSent, c.BytesSent, now.Sub(t.networkAt).Seconds())
			}
		}
		if !t.networkAt.IsZero() {
			p.RX, p.TX = &rx, &tx
		}
		t.network, t.networkAt = next, now
	} else {
		d.Unavailable = append(d.Unavailable, "network")
	}
	if t.diskPending != nil {
		select {
		case result := <-t.diskPending:
			d.Disks, d.DisksAt = result.disks, result.at
			t.diskPartial = result.partial
			t.diskPending = nil
		default:
		}
	}
	if t.diskPending == nil && time.Since(d.DisksAt) > 30*time.Second {
		t.diskPending = make(chan dashboardDiskResult, 1)
		go collectDashboardDisks(t.diskPending)
	}
	if len(d.Disks) == 0 || t.diskPartial || time.Since(d.DisksAt) > 45*time.Second {
		d.Unavailable = append(d.Unavailable, "disk")
	}
	if time.Since(d.ProcessesAt) > 30*time.Second {
		if ps, err := process.ProcessesWithContext(ctx); err == nil {
			procs := []dashboardProcess{}
			for _, proc := range ps {
				if ctx.Err() != nil {
					break
				}
				if m, err := proc.MemoryInfoWithContext(ctx); err == nil && m.RSS > 0 {
					name, _ := proc.NameWithContext(ctx)
					procs = append(procs, dashboardProcess{proc.Pid, name, m.RSS})
				}
			}
			sort.Slice(procs, func(i, j int) bool { return procs[i].RSS > procs[j].RSS })
			if len(procs) > 8 {
				procs = procs[:8]
			}
			if ctx.Err() == nil {
				d.Processes, d.ProcessesAt = procs, now
			}
		}
	}
	if d.ProcessesAt.IsZero() {
		d.Unavailable = append(d.Unavailable, "processes")
	}
	d.SampledAt = now
	d.History = append(d.History, p)
	if len(d.History) > 60 {
		d.History = d.History[len(d.History)-60:]
	}
	t.data = d
	t.mu.Unlock()
	writeJSON(w, 200, d)
}

// All business counts and time buckets use one PostgreSQL statement/snapshot.
// The scope is explicitly online rows (cold archives are not mixed into hourly
// data, since archives retain daily aggregates rather than individual calls).
func (s *Server) dashboardOverview(w http.ResponseWriter, r *http.Request) {
	days := atoiDefault(r.URL.Query().Get("days"), 7)
	if days != 7 && days != 14 && days != 30 {
		writeErr(w, 400, "days must be 7, 14 or 30")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	var payload json.RawMessage
	err := s.m.pg.QueryRowContext(ctx, dashboardOverviewSQL, days).Scan(&payload)
	if err != nil {
		writeErr(w, 500, "dashboard aggregation failed")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, payload)
}

const dashboardOverviewSQL = `
WITH scope AS (
 SELECT * FROM llm_usage WHERE ts >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - (($1::int-1)*interval '1 day') AND ts <= now()
), models AS (
 SELECT COALESCE(NULLIF(model,''),'unknown') model, count(*) calls,
 coalesce(sum(input_tokens),0) input, coalesce(sum(output_tokens),0) output,
 coalesce(sum(cache_read),0) cache, coalesce(sum(cache_write),0) cache_write,
 coalesce(avg(latency_ms),0) latency FROM scope GROUP BY 1
), hourly AS (
 SELECT to_char(ts AT TIME ZONE 'UTC','YYYY-MM-DD') AS day, extract(hour from ts AT TIME ZONE 'UTC')::int AS hour,
 count(*) calls, sum(input_tokens) input, sum(output_tokens) output, sum(cache_read) cache FROM scope GROUP BY 1,2
), classes AS (
 SELECT severity, vulnclass, count(*) count, min(id) example_id, json_agg(id ORDER BY id) ids FROM findings GROUP BY 1,2
), asset_types AS (SELECT type,count(*) count FROM assets GROUP BY type),
 activity_days AS (
 SELECT to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD') AS day,count(*) count FROM activity
 WHERE created_at>=date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'-interval '364 days' AND created_at<=now() GROUP BY 1
)
SELECT json_build_object(
 'sampled_at',now(),'timezone','UTC','days',$1::int,
 'findings',coalesce((SELECT json_agg(classes ORDER BY CASE severity WHEN 'critical' THEN 0 WHEN 'high' THEN 1 WHEN 'medium' THEN 2 WHEN 'low' THEN 3 ELSE 4 END,count DESC,vulnclass) FROM classes),'[]'::json),
 'assets',coalesce((SELECT json_agg(asset_types ORDER BY count DESC,type) FROM asset_types),'[]'::json),
 'models',coalesce((SELECT json_agg(models ORDER BY input+output DESC,model) FROM models),'[]'::json),
 'hourly',coalesce((SELECT json_agg(hourly ORDER BY day,hour) FROM hourly),'[]'::json),
 'activity',coalesce((SELECT json_agg(activity_days ORDER BY day) FROM activity_days),'[]'::json)
)`
