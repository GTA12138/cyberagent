// algo_api.go 预置算法库的管理 API（前端「算法库」页的数据面）。
//
// 与工具面（apply_algo/save_algo，给 agent）互补：这里是人管算法的地方——
// 任务启动前上传/更新预置算法、试跑验证、停用。鉴权与其余 /api 相同（全局
// 中间件）；写操作走 upsert（同名覆盖，算法迭代不堆版本）。
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"chengjian.local/chengjian/db"
)

// GET /api/algorithms — 全量列表（含停用；管理页要看到全貌）。?enabled=1 只取启用。
func (s *Server) algorithmsList(w http.ResponseWriter, r *http.Request) {
	st := s.algoStore()
	if st == nil {
		writeErr(w, 503, "DB 未就绪")
		return
	}
	items, err := st.List(r.Context(), r.URL.Query().Get("enabled") != "1")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "count": len(items)})
}

// postAlgorithmBody 是创建/更新的请求体（verify_input 可选：保存前实跑验证）。
type postAlgorithmBody struct {
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	MatchHost   string          `json:"match_host"`
	MatchPath   string          `json:"match_path"`
	Description string          `json:"description"`
	Script      string          `json:"script"`
	Enabled     *bool           `json:"enabled"`
	VerifyInput json.RawMessage `json:"verify_input"`
}

// POST /api/algorithms — 新建/更新（按 name upsert）。带 verify_input 时先实跑，
// 契约跑不通直接 400——不能把坑留给任务里的 agent。
func (s *Server) algorithmsUpsert(w http.ResponseWriter, r *http.Request) {
	st := s.algoStore()
	if st == nil {
		writeErr(w, 503, "DB 未就绪")
		return
	}
	var b postAlgorithmBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if b.Name == "" || b.Script == "" || b.Description == "" {
		writeErr(w, 400, "name / description / script 必填")
		return
	}
	switch b.Kind {
	case "sign", "slider", "encrypt", "decrypt", "other":
	default:
		writeErr(w, 400, "kind 必须是 sign/slider/encrypt/decrypt/other")
		return
	}
	verified := false
	if len(b.VerifyInput) > 0 {
		if _, verr := runAlgorithmScript(r.Context(), b.Script, b.VerifyInput); verr != nil {
			writeErr(w, 400, "verify_input 实跑失败（未保存）："+verr.Error())
			return
		}
		verified = true
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	id, err := st.UpsertAlgorithm(r.Context(), &db.Algorithm{
		Name: b.Name, Kind: b.Kind, MatchHost: b.MatchHost, MatchPath: b.MatchPath,
		Description: b.Description, Script: b.Script, Enabled: enabled,
	})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	log.Printf("[algo] 算法 upsert：name=%s kind=%s verified=%v", b.Name, b.Kind, verified)
	writeJSON(w, 200, map[string]any{"id": id, "name": b.Name, "verified": verified})
}

// POST /api/algorithms/{id}/test {input:{...}} — 试跑（不落库）。
func (s *Server) algorithmsTest(w http.ResponseWriter, r *http.Request) {
	st := s.algoStore()
	if st == nil {
		writeErr(w, 503, "DB 未就绪")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var b struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || len(b.Input) == 0 {
		writeErr(w, 400, "body 需要 input 对象")
		return
	}
	items, lerr := st.List(r.Context(), true)
	if lerr != nil {
		writeErr(w, 500, lerr.Error())
		return
	}
	var algo *db.Algorithm
	for i := range items {
		if items[i].ID == id {
			// List 目录不带 script 正文，按名取全量。
			a, gerr := st.Get(r.Context(), items[i].Name)
			if gerr == nil {
				algo = a
			}
			break
		}
	}
	if algo == nil {
		writeErr(w, 404, fmt.Sprintf("算法 #%d 不存在", id))
		return
	}
	out, rerr := runAlgorithmScript(r.Context(), algo.Script, b.Input)
	if rerr != nil {
		writeErr(w, 400, rerr.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"output": json.RawMessage(out)})
}

// DELETE /api/algorithms/{id} — 停用（软删除：成果保留，可再启用）。
func (s *Server) algorithmsDisable(w http.ResponseWriter, r *http.Request) {
	st := s.algoStore()
	if st == nil {
		writeErr(w, 503, "DB 未就绪")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := st.SetEnabled(r.Context(), id, false); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.auditIntranet(r, "algorithm disable id=%d", id)
	writeJSON(w, 200, map[string]any{"id": id, "enabled": false})
}

// POST /api/algorithms/{id}/enable — 重新启用。
func (s *Server) algorithmsEnable(w http.ResponseWriter, r *http.Request) {
	st := s.algoStore()
	if st == nil {
		writeErr(w, 503, "DB 未就绪")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := st.SetEnabled(r.Context(), id, true); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "enabled": true})
}
