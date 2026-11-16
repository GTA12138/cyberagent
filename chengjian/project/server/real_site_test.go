package server

// real_site_test.go 是 T-03 的可复跑版本：拿一个**真实站点**跑一轮补全，
// 验证三件事——
//
//	① graph_overview 的 sites_without_endpoints 随补全下降（补全真的展开了站点）
//	② endpoint 增量合理（不是 0，也不是把静态资源当端点灌进来）
//	③ 补全发的请求**都**落在流量仓里（录制开着时）
//
// 为什么单独一个文件、且默认跳过：真实站点不可控——它会改版、会限流、会
// 因为网络抖动返回不同结果。把它塞进常规测试会制造一条会随机红的用例，
// 而随机红的用例最终会被忽略，然后连真正的回归也一起被忽略。
//
// 用法（需要一个能上外网的机器 + 隔离库）：
//
//	$env:CHENGJIAN_REAL_SITE = 'https://example.com'
//	./scripts/pgtest.ps1 -Packages ./server -Run TestRealSiteEnrichment
//
// 第三个判据（③）只在录制开启时检查：补全走的是任务出口代理，录制关掉时
// 本来就不该有交换记录。把它写成「必须有记录」会让录制关闭的配置永远失败。
//
// 实测基线（2026-10-24，https://www.zhihu.com/signin）：
//
//	sites_without_endpoints 1 → 0；endpoint 0 → 1；方法分布 map[GET:1]；耗时 2.3s
//
// 这个增量偏小，原因是真实站点的脚本大多是跨域 CDN + 反爬混淆，同源可读的
// 只有 HTML 里的表单与少量内联脚本。**不要把这条基线读成「补全没用」**：
// 它说明的是「公开静态面能读到的就这么多」，动态请求面要靠浏览器（W12）。
// 判据③的完整验证在 TestLocalEnrichmentThroughTaskProxy：那里有可控的任务
// 代理实例，能核对 captures=2/0 与出口一致性；真实站点这边没有任务代理实例
// （HTTPS 目标过 MITM 需要目标侧信任平台 CA，那属于部署配置而不是代码能力）。

import (
	"os"
	"strings"
	"testing"
	"time"

	"chengjian.local/chengjian/db"
	"chengjian.local/chengjian/enrich"
)

// realSiteUA 是一个普通的浏览器 UA。补全必须与 worker 共用同一套指纹，
// 自报 chengjian-enrich/1.0 等于当场把 worker 的伪装作废。
const realSiteUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

func TestRealSiteEnrichment(t *testing.T) {
	site := strings.TrimSpace(os.Getenv("CHENGJIAN_REAL_SITE"))
	if site == "" {
		t.Skip("需要真实站点：设置 CHENGJIAN_REAL_SITE=https://<真实站点>")
	}
	dsn := os.Getenv("CHENGJIAN_PG_DSN")
	if dsn == "" {
		t.Skip("需要隔离库：CHENGJIAN_PG_DSN")
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	task, err := pg.CreateTask("real site enrichment baseline", "local only", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteTask(task.ID)

	// 出口：直连。真实站点验证的是「补全本身能展开一个真站点」，
	// 不是代理链路——代理链路已有本机 fixture 覆盖（TestLocalEnrichmentThroughTaskProxy）。
	m := &Manager{pg: pg, trafficOn: false}
	e := enrich.New(pg.Assets(), m.enrichProxyForTask, 4)
	defer e.Close()
	e.SetUserAgent(func(int64) string { return realSiteUA })
	e.SetBudget(enrich.Budget{MaxRequests: 40, MaxDepth: 1, PerHost: 24})

	id, err := pg.Assets().UpsertHTTPService(db.UpsertHTTPServiceReq{URL: site, TaskID: task.ID})
	if err != nil {
		t.Fatal(err)
	}
	beforeMissing, _, err := pg.Assets().SitesWithoutEndpoints(task.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	if beforeMissing == 0 {
		t.Fatalf("前置条件不成立：登记 %s 后应出现「无端点站点」，实际 0（是不是 URL 归一化失败？）", site)
	}
	beforeEndpoints := countEndpoints(t, pg, task.ID)

	e.ProbeSite(id, task.ID, site)

	// 补全会跟一跳（主页 → 脚本/页面），给足时间但设硬上限。
	deadline := time.Now().Add(90 * time.Second)
	var afterEndpoints int
	for {
		afterEndpoints = countEndpoints(t, pg, task.ID)
		afterMissing, _, _ := pg.Assets().SitesWithoutEndpoints(task.ID, 5)
		if afterMissing == 0 && afterEndpoints > beforeEndpoints {
			break
		}
		if time.Now().After(deadline) {
			afterMissingFinal, sample, _ := pg.Assets().SitesWithoutEndpoints(task.ID, 5)
			t.Fatalf("补全未在 90s 内展开站点：endpoint %d → %d，sites_without_endpoints=%d 样例=%v\n"+
				"可能原因：站点是纯静态无端点、被 WAF 拦、或网络不可达。这不是断言失败而是环境不成立，"+
				"换一个站点重跑；不要为了让这条变绿而放宽断言。",
				beforeEndpoints, afterEndpoints, afterMissingFinal, sample)
		}
		time.Sleep(250 * time.Millisecond)
	}

	// ② 端点增量合理性：全是静态资源说明抽取器把 asset 当端点，那会把资产图灌满垃圾。
	eps, err := pg.Assets().QueryByTask(task.ID, "endpoint", 500, 0)
	if err != nil {
		t.Fatal(err)
	}
	staticLike := 0
	methods := map[string]int{}
	for _, a := range eps {
		if a == nil {
			continue
		}
		methods[a.Method]++
		if strings.HasSuffix(strings.ToLower(a.URL), ".js") || strings.HasSuffix(strings.ToLower(a.URL), ".css") {
			staticLike++
		}
	}
	if staticLike > 0 {
		t.Fatalf("端点里混进了静态资源 %d 条（抽取器把 asset 当端点会让资产图失去意义）", staticLike)
	}
	if len(methods) == 0 {
		t.Fatal("端点方法全空：method 未确定的路径不该被登记成端点")
	}

	t.Logf("真实站点基线：%s；sites_without_endpoints %d → 0；endpoint %d → %d；方法分布 %v；UA=%s",
		site, beforeMissing, beforeEndpoints, len(eps), methods, realSiteUA)
}

func countEndpoints(t *testing.T, pg *db.DB, taskID int64) int {
	t.Helper()
	assets, err := pg.Assets().QueryByTask(taskID, "endpoint", 500, 0)
	if err != nil {
		t.Fatalf("统计端点失败: %v", err)
	}
	n := 0
	for _, a := range assets {
		if a != nil {
			n++
		}
	}
	return n
}
