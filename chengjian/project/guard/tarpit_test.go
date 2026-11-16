package guard

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// newTarpitGuard builds a Guard with a tarpit budget of n and a hint collector.
func newTarpitGuard(budget int) (*Guard, *[]string) {
	g := New()
	hints := &[]string{}
	var mu sync.Mutex
	g.SetTarpit(TarpitConfig{
		Budget: func() int { return budget },
		Hint: func(host string, count int) {
			mu.Lock()
			defer mu.Unlock()
			*hints = append(*hints, host)
		},
	})
	return g, hints
}

func bashEvent(t *testing.T, g *Guard, command string) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"command": command})
	g.Hooks().PreToolUse(context.Background(), "Bash", input)
}

func webFetchEvent(t *testing.T, g *Guard, url string) {
	t.Helper()
	input, _ := json.Marshal(map[string]string{"url": url})
	g.Hooks().PreToolUse(context.Background(), "WebFetch", input)
}

// 补全不是唯一会自己发请求的东西:Bash 里起的抓取类二进制同样烧预算,
// 之前只统计 WebFetch,它们是一条完全静默的通道。
func TestTarpitCountsBashFetchers(t *testing.T) {
	g, hints := newTarpitGuard(2)
	bashEvent(t, g, "curl -s https://maze.example/a")
	bashEvent(t, g, "wget -qO- https://maze.example/b")
	if len(*hints) != 0 {
		t.Fatalf("预算内不应熔断, got %v", *hints)
	}
	bashEvent(t, g, "katana -u https://maze.example/c")
	if len(*hints) != 1 || (*hints)[0] != "maze.example" {
		t.Fatalf("第 3 次抓取应熔断一次并报 maze.example, got %v", *hints)
	}
	// 熔断后继续抓:不重复写 hint(每 host 只一次)。
	bashEvent(t, g, "curl https://maze.example/d")
	if len(*hints) != 1 {
		t.Fatalf("同一 host 只应熔断一次, got %v", *hints)
	}
}

// 端口扫描/SSH/数据库客户端不产生页面抓取,算进来只会误触发。
func TestTarpitIgnoresNonFetchBinaries(t *testing.T) {
	g, hints := newTarpitGuard(1)
	for i := 0; i < 5; i++ {
		bashEvent(t, g, "nmap -sV -p 1-1000 10.0.0.5")
		bashEvent(t, g, "ssh root@10.0.0.6")
		bashEvent(t, g, "psql -h 10.0.0.7 -U app")
	}
	if len(*hints) != 0 {
		t.Fatalf("非抓取命令不应计入 tarpit, got %v", *hints)
	}
}

// 解释器只按显式 scheme URL 计数:无法从 `python3 x.py --target 10.0.0.5`
// 判断它在抓取还是在利用,宁缺勿滥。
func TestTarpitScriptRunnerNeedsScheme(t *testing.T) {
	g, hints := newTarpitGuard(1)

	// 裸 IP、无 scheme:不计。
	for i := 0; i < 3; i++ {
		bashEvent(t, g, "python3 exploit.py --target 10.0.0.5")
	}
	if len(*hints) != 0 {
		t.Fatalf("无 scheme 的脚本调用不应计入, got %v", *hints)
	}

	// 带 scheme:计。
	bashEvent(t, g, "python3 harvest_static.py https://maze.example")
	bashEvent(t, g, "python3 harvest_static.py https://maze.example")
	if len(*hints) != 1 || (*hints)[0] != "maze.example" {
		t.Fatalf("带 scheme 的脚本抓取应计入并熔断, got %v", *hints)
	}
}

// enrich 侧上报走 NoteFetch 直连:同一条预算,同一个熔断,来源标 enrich。
func TestNoteFetchReportsEnrichSource(t *testing.T) {
	g, hints := newTarpitGuard(2)
	g.NoteFetch("enrich", "maze.example", 1)
	g.NoteFetch("enrich", "maze.example", 1)
	if len(*hints) != 0 {
		t.Fatalf("预算内不应熔断, got %v", *hints)
	}
	g.NoteFetch("enrich", "maze.example", 1)
	if len(*hints) != 1 || (*hints)[0] != "maze.example" {
		t.Fatalf("补全超预算应熔断一次, got %v", *hints)
	}
	var found bool
	for _, a := range g.Audit() {
		if a.Tool == "enrich" && a.Reason != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("熔断审计行应标明来源 enrich,便于事后区分是 agent 抓的还是补全抓的")
	}
}

// WebFetch 与 enrich 共用同一份计数:两条通道各自都不到预算,合起来超了也要熔断。
func TestTarpitSharesBudgetAcrossSources(t *testing.T) {
	g, hints := newTarpitGuard(2)
	webFetchEvent(t, g, "https://maze.example/a")
	g.NoteFetch("enrich", "maze.example", 1)
	if len(*hints) != 0 {
		t.Fatalf("合计 2 次不应熔断, got %v", *hints)
	}
	g.NoteFetch("enrich", "maze.example", 1)
	if len(*hints) != 1 {
		t.Fatalf("合计 3 次超过预算 2 应熔断, got %v", *hints)
	}
}

// 未装配 tarpit / 空 Guard 时静默跳过,不能 panic——enrich 的 worker 会在任意
// 任务上下文中调用它。
func TestNoteFetchWithoutTarpitIsNoop(t *testing.T) {
	g := New() // 未 SetTarpit
	g.NoteFetch("enrich", "maze.example", 10)
	var nilGuard *Guard
	nilGuard.NoteFetch("enrich", "maze.example", 10)
	// 空 host / 非正数次数同样跳过。
	g.NoteFetch("enrich", "", 1)
	g.NoteFetch("enrich", "maze.example", 0)
}

// 并发计数不能丢:丢一次计数就等于预算被放宽一次。
func TestNoteFetchConcurrentKeepsExactCount(t *testing.T) {
	g, hints := newTarpitGuard(100)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g.NoteFetch("enrich", "maze.example", 1)
		}()
	}
	wg.Wait()
	if len(*hints) != 0 {
		t.Fatalf("正好 100 次等于预算,不应熔断, got %v", *hints)
	}
	g.NoteFetch("enrich", "maze.example", 1)
	if len(*hints) != 1 {
		t.Fatalf("第 101 次应熔断——计数若丢过就不会触发, got %v", *hints)
	}
}

// fetchTargets 的边界:抽不出 host 就不计,避免把无关命令算成抓取。
func TestFetchTargets(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		want []string
	}{
		{"curl url", "curl -s https://a.example/x", []string{"a.example"}},
		{"wrapped in bash -c", `bash -c "curl https://b.example"`, []string{"b.example"}},
		{"httpx scheme", "httpx -u https://c.example", []string{"c.example"}},
		// 已知保守跳过:httpx 的 -u 是 URL,但 curl 的 -u 是凭据(valueFlags),
		// 同名字段无法两全,按「宁缺勿滥」跳过裸主机名。
		{"httpx bare host skipped", "httpx -u c.example", nil},
		{"curl -u creds + url", "curl -u a:b https://e.example", []string{"e.example"}},
		{"python with scheme", "python3 s.py https://d.example", []string{"d.example"}},
		{"python bare ip", "python3 s.py --target 10.0.0.5", nil},
		{"nmap", "nmap -sV 10.0.0.5", nil},
		{"empty", "", nil},
		{"no url at all", "ls -la /tmp", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fetchTargets(c.cmd)
			if len(got) != len(c.want) {
				t.Fatalf("fetchTargets(%q) = %v, want %v", c.cmd, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("fetchTargets(%q) = %v, want %v", c.cmd, got, c.want)
				}
			}
		})
	}
}
