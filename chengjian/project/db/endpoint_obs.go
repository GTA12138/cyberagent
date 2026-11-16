// endpoint_obs.go 端点参数观测（W2 数据地基）。
//
// 回答的问题：「这个端点的这个参数，都被什么身份/什么请求用过什么值」——
// 这是 IDOR/BFLA 差分测试的用例来源：参数值集合里有别人的 id、同一个参数
// 出现过不同身份的取值，就是该做差分的信号。
//
// 落库口径（刻意收窄，防两张皮）：
//   - 只从平台自己的受管动作抽取（http_fetch / http_replay 的基线与变体），
//     不挂 MITM 录制热路径——录制是每请求必经之路，加同步 DB 写会把录制
//     延迟和任务执行耦合；受管动作已覆盖 agent 的结构化发包来源，Bash curl
//     的流量本来也进录制库可供事后回溯。
//   - 值截断 128 字符；authorization/cookie 类头不落值（只留名字），敏感面
//     不因观测而扩大。
//   - UNIQUE(task,method,url,name,loc,value) 幂等累 hits：观测是频次事实，
//     不是事件流。
package db

import (
	"strings"
	"time"
)

// EndpointObs 是一条参数观测。
type EndpointObs struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	Method    string    `json:"method"`
	URL       string    `json:"url"`  // 不带 query 的绝对 URL（参数单列）
	Name      string    `json:"name"` // 参数名
	Loc       string    `json:"loc"`  // query | body
	Value     string    `json:"value"`
	Hits      int       `json:"hits"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// EndpointObsInput 是一次抽取的最小输入（UpsertEndpointObs 批量收）。
type EndpointObsInput struct {
	Method string
	URL    string // 不带 query
	Name   string
	Loc    string // query | body
	Value  string
}

const endpointObsCols = `id, task_id, method, url, name, loc, value, hits, first_seen, last_seen`

// MaxObsValueLen 是参数值截断长度。
const MaxObsValueLen = 128

// NormalizeEndpointObs 清洗一条观测：截值、去空白；空名/空 URL/非法 loc 丢弃。
func NormalizeEndpointObs(in EndpointObsInput) (EndpointObsInput, bool) {
	in.Name = strings.TrimSpace(in.Name)
	in.URL = strings.TrimSpace(in.URL)
	in.Method = strings.ToUpper(strings.TrimSpace(in.Method))
	in.Loc = strings.ToLower(strings.TrimSpace(in.Loc))
	in.Value = strings.TrimSpace(in.Value)
	if len(in.Value) > MaxObsValueLen {
		in.Value = in.Value[:MaxObsValueLen]
	}
	if in.Name == "" || in.URL == "" || in.Method == "" {
		return in, false
	}
	if in.Loc != "query" && in.Loc != "body" {
		return in, false
	}
	return in, true
}

// UpsertEndpointObs 批量幂等落库：新插入、已有则 hits+1 刷 last_seen。
// taskID<=0 时 no-op（无任务上下文的观测没有归属，不落）。
func (s *AssetStore) UpsertEndpointObs(taskID int64, ins []EndpointObsInput) (int, error) {
	if taskID <= 0 || len(ins) == 0 {
		return 0, nil
	}
	written := 0
	for _, in := range ins {
		in, ok := NormalizeEndpointObs(in)
		if !ok {
			continue
		}
		var id int64
		err := s.db.QueryRow(`
INSERT INTO endpoint_observation(task_id, method, url, name, loc, value)
VALUES ($1,$2,$3,$4,$5,$6)
ON CONFLICT (task_id, method, url, name, loc, value)
DO UPDATE SET hits = endpoint_observation.hits + 1, last_seen = now()
RETURNING id`, taskID, in.Method, in.URL, in.Name, in.Loc, in.Value).Scan(&id)
		if err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// EndpointObsView 是查询返回的聚合行：参数 × 值 × 频次。
type EndpointObsView struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	Name   string `json:"name"`
	Loc    string `json:"loc"`
	Values []struct {
		Value string `json:"value"`
		Hits  int    `json:"hits"`
	} `json:"values"`
}

// ListEndpointObs 按任务列出参数观测（可选 url 子串过滤），按 URL 聚合值集合。
// limit 是原始行上限（默认 500）。
func (s *AssetStore) ListEndpointObs(taskID int64, urlContains string, limit int) ([]EndpointObsView, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(`
SELECT method, url, name, loc, value, hits FROM endpoint_observation
WHERE task_id=$1 AND ($2='' OR url ILIKE '%'||$2||'%')
ORDER BY url, name, hits DESC LIMIT $3`, taskID, strings.TrimSpace(urlContains), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EndpointObsView{}
	idx := map[string]int{}
	for rows.Next() {
		var m, u, n, l, v string
		var h int
		if err := rows.Scan(&m, &u, &n, &l, &v, &h); err != nil {
			return nil, err
		}
		key := m + " " + u + " " + n + " " + l
		i, ok := idx[key]
		if !ok {
			out = append(out, EndpointObsView{Method: m, URL: u, Name: n, Loc: l})
			i = len(out) - 1
			idx[key] = i
		}
		out[i].Values = append(out[i].Values, struct {
			Value string `json:"value"`
			Hits  int    `json:"hits"`
		}{Value: v, Hits: h})
	}
	return out, rows.Err()
}
