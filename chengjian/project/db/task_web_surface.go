package db

// 本文件补一个「采集侧」的态势信号：任务范围内已经登记为 Web 服务、但同域下
// 一条 endpoint 都没有的站点。
//
// 为什么它必须存在：agent/planner.go 的态势说明里已经把这个字段写进契约
// （"sites_without_endpoints（无端点的站点，提示可能待探的方向）"），但
// graph_overview 从来没产出过它——提示词在描述一个不存在的数据，planner 因此
// 长期基于空信号做决策。这里把它补成真实查询。
//
// 语义边界：这是**资产结构**信号（哪些站点还没被展开），不是**测试覆盖度**
// （哪些资产还没被验证）。所以它不受 tasks.coverage_enabled 影响——覆盖度开关
// 关掉只是不再把资产当分母算百分比，不代表「这个站点没有端点」这件事消失。

// SitesWithoutEndpoints 返回任务范围内「有 service、但同域无 endpoint」的站点。
// 返回总数（可能远大于 sample 长度）与最多 limit 条样例 URL。
//
// 判定用 domain 对齐而不是 URL 前缀：endpoint 的 url 可能带不同的路径前缀
// （/api/v1/x 与 /api/v2/x），前缀匹配会漏；domain 是两侧都由 parseURL 归一化
// 出来的同一个键，更稳。
func (s *AssetStore) SitesWithoutEndpoints(taskID int64, limit int) (int, []string, error) {
	if taskID <= 0 {
		return 0, nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	const base = `
FROM assets s
WHERE s.type = 'service'
  AND s.url <> ''
  AND s.domain <> ''
  AND $1 = ANY(s.task_ids)
  AND NOT EXISTS (
        SELECT 1 FROM assets e
        WHERE e.type = 'endpoint'
          AND $1 = ANY(e.task_ids)
          AND e.domain = s.domain
      )`
	var total int
	if err := s.db.QueryRow(`SELECT count(DISTINCT s.domain)`+base, taskID).Scan(&total); err != nil {
		return 0, nil, err
	}
	if total == 0 {
		return 0, nil, nil
	}
	rows, err := s.db.Query(`SELECT min(s.url) AS url`+base+`
GROUP BY s.domain
ORDER BY url
LIMIT $2`, taskID, limit)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	out := make([]string, 0, limit)
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return 0, nil, err
		}
		out = append(out, u)
	}
	return total, out, rows.Err()
}
