package guard

// 批 5 B3(设计文档 HONEYPOT-DETECTION-DESIGN.md 二-B「自身防护」第 5 条):tarpit 熔断。
// Nepenthes / Cloudflare AI Labyrinth 类反爬虫迷宫用无限假页面困死抓取方、烧光
// 任务预算。本文件按「每任务每 host」计数 WebFetch 抓取调用,超过预算触发一次
// 熔断:经 Hint 回调写 hint 节点进探索图(planner 每轮 graph overview 必读 hints,
// 参照停滞检测 hint 的既有通道),提示停派该方向;之后对该 host 的抓取仍 warn
// 放行——不硬 block,防误杀真实大站。
//
// 计数器在内存里(每任务一个 Guard,见 server taskFromPG),任务重启清零:重启后
// 重新爬一遍才会再触发,代价可接受,不落库。

import (
	"encoding/json"
	"log"
	"net/url"
	"strconv"
	"strings"

	"github.com/Autumn-27/norma/hook"
)

// defaultTarpitFetchBudget 是每 host 抓取调用的默认预算(settings 键
// tarpit_fetch_budget 可配,<=0 或非法值回落到它)。
const defaultTarpitFetchBudget = 40

// TarpitConfig 是 tarpit 熔断的装配:两个回调都由 server 注入(guard 不反向
// 依赖 db/server)。
type TarpitConfig struct {
	// Budget 返回当前预算(settings tarpit_fetch_budget)。nil 或返回 <=0 → 默认 40。
	Budget func() int
	// Hint 是熔断触发(每 host 只一次)时的回调,server 装配为写探索图 hint 节点。
	// nil = 只记 guard 审计/日志。
	Hint func(host string, count int)
}

// tarpitState 是每 Guard(=每任务)的 tarpit 计数状态;经 Guard.mu 串行化。
type tarpitState struct {
	cfg    TarpitConfig
	counts map[string]int
	fired  map[string]bool // 每 host 只熔断一次
}

// SetTarpit 装配 tarpit 熔断。零值/再传 nil 回调只是降级(不写 hint),不报错。
func (g *Guard) SetTarpit(cfg TarpitConfig) {
	g.mu.Lock()
	g.tarpit = &tarpitState{cfg: cfg, counts: map[string]int{}, fired: map[string]bool{}}
	g.mu.Unlock()
}

func (c TarpitConfig) budget() int {
	if c.Budget == nil {
		return defaultTarpitFetchBudget
	}
	if b := c.Budget(); b > 0 {
		return b
	}
	return defaultTarpitFetchBudget
}

// checkTarpit 统计「对同一 host 的抓取调用」;超预算时熔断一次(写 hint),
// 之后对该 host 一律 warn 放行(只记审计,不 block)。
//
// 三条来源都算,否则预算形同虚设:
//  1. agent 侧 WebFetch(本函数直接解析 url);
//  2. agent 侧 Bash 里起的抓取类二进制(curl/katana/httpx/ffuf…,见 fetchTargets);
//  3. 引擎侧补全(enrich 经 SetFetchObserver → Guard.NoteFetch 上报)。
//
// 第 3 条是 2026-10-23 补的:补全会自己发请求(登记 service 抓一次 + 派生跟一跳),
// 之前只统计 WebFetch,补全等于一条预算外通道——而且它恰好是「一个导航页派生十几个
// 脚本」这种最容易撞上 tarpit 的形态。
func (g *Guard) checkTarpit(ev hook.Event, cmd string) {
	if ev.ToolName == "WebFetch" {
		var in struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(ev.Input, &in) != nil || in.URL == "" {
			return
		}
		u, err := url.Parse(in.URL)
		if err != nil || u.Hostname() == "" {
			return
		}
		g.NoteFetch(ev.ToolName, u.Hostname(), 1)
		return
	}
	if cmd == "" {
		return
	}
	for _, host := range fetchTargets(cmd) {
		g.NoteFetch(ev.ToolName, host, 1)
	}
}

// NoteFetch 记 n 次对 host 的抓取,并在超预算时熔断一次(写 hint)。
// 并发安全:agent 侧 PreToolUse 与 enrich 的后台 worker 会同时调它。
// source 只用于审计行(WebFetch / Bash / enrich),不参与预算判断。
func (g *Guard) NoteFetch(source, host string, n int) {
	if g == nil || host == "" || n <= 0 {
		return
	}
	if source == "" {
		source = "tarpit"
	}
	g.mu.Lock()
	st := g.tarpit
	if st == nil {
		g.mu.Unlock()
		return
	}
	st.counts[host] += n
	count := st.counts[host]
	budget := st.cfg.budget()
	over := count > budget
	first := over && !st.fired[host]
	if first {
		st.fired[host] = true
	}
	hint := st.cfg.Hint
	g.mu.Unlock()

	if !over {
		return
	}
	if first {
		// 熔断:记审计 + 回调写 hint(host/次数),提示 planner 停派该方向。
		g.record(source, "allow", tarpitMessage(host, count, budget), "")
		log.Printf("[guard][tarpit] host %s 抓取调用 %d 次超过预算 %d,已熔断告警(warn 放行) 来源=%s", host, count, budget, source)
		if hint != nil {
			hint(host, count)
		}
		return
	}
	// 已熔断过的 host:warn 放行——不硬 block(防误杀真实大站),但每次留一条
	// 轻量审计,事后能还原「熔断后仍在抓」的行为。
	g.record(source, "allow", "tarpit 熔断后仍抓取 "+host, "")
}

// fetchBinaries 是「以目标 URL 为位置参数、会真的发起 HTTP(S) 抓取」的命令名。
// 刻意不收端口扫描/SSH/数据库客户端(nmap/nc/ssh/psql):它们不产生页面抓取,
// 算进来只会误触发熔断。
var fetchBinaries = map[string]bool{
	"curl": true, "wget": true, "httpx": true, "katana": true,
	"ffuf": true, "gobuster": true, "feroxbuster": true, "dirsearch": true,
	"dirb": true, "wfuzz": true, "nikto": true, "whatweb": true,
	"nuclei": true, "wpscan": true, "gospider": true, "hakrawler": true,
}

// scriptRunners 是「可能在做抓取、也可能在做别的事」的解释器。它们只按命令里
// 显式带 scheme 的 URL 计数(见 fetchTargets),因为
// `python3 exploit.py --target 10.0.0.5` 无法判断是抓取还是利用——宁缺勿滥。
var scriptRunners = map[string]bool{
	"python": true, "python3": true, "node": true, "nodejs": true,
}

// fetchTargets 从一条 shell 命令里抽出「本次会被抓取的 host」。
// 抽不出 = 不计(保守取向,与 ExtractHosts 一致)。
func fetchTargets(cmd string) []string {
	if cmd == "" {
		return nil
	}
	strict, script := false, false
	for _, tok := range reShellSplit.Split(cmd, -1) {
		b := baseOf(strings.Trim(tok, `"'`))
		if fetchBinaries[b] {
			strict = true
			break
		}
		if scriptRunners[b] {
			script = true
		}
	}
	switch {
	case strict:
		return ExtractHosts(cmd)
	case script:
		var out []string
		seen := map[string]bool{}
		for _, raw := range reScheme.FindAllString(cmd, -1) {
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" {
				continue
			}
			h := strings.ToLower(u.Hostname())
			if seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
		return out
	}
	return nil
}

// tarpitMessage 是熔断审计/hint 共用的文案:疑似 tarpit 迷宫(host,次数)。
func tarpitMessage(host string, count, budget int) string {
	return "疑似 tarpit 迷宫(" + host + ",抓取 " + strconv.Itoa(count) + " 次,预算 " + strconv.Itoa(budget) + ")"
}
