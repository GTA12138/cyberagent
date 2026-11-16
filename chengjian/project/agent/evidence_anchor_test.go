// evidence_anchor_test.go 证据切片锚定（P4-1 prompt 接线）的判定测试：
// 真片段锚定成功且 hash 可验；伪造片段（不在响应里）拒绝上报；取体失败拒绝；
// 未装配取体回调时保持未锚定（渐进增强）。
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
)

func anchorBodyFn(body string, err error) func(context.Context, string) (string, error) {
	return func(_ context.Context, _ string) (string, error) { return body, err }
}

func TestAnchorEvidenceRefsAnchorsRealSnippet(t *testing.T) {
	body := "HTTP/1.1 200 OK\r\n\r\n{\"user\":1,\"role\":\"admin\",\"debug\":\"root:x:0:0\"}"
	ts := NewToolSet(nil, "worker")
	ts.SetTrafficBodyResolver(anchorBodyFn(body, nil))
	refs := []db.TrafficRef{{TrafficID: "t1", Role: "proof"}}
	out, note, err := ts.anchorEvidenceRefs(context.Background(), refs,
		map[string]string{"t1": "\"role\":\"admin\""})
	if err != nil {
		t.Fatalf("真实片段应锚定成功: %v", err)
	}
	if !out[0].Anchored() || out[0].SliceHash == "" {
		t.Fatalf("引用应带字节区间与 hash: %+v", out[0])
	}
	if ok, verr := db.VerifySlice(out[0], []byte(body)); !ok || verr != nil {
		t.Fatalf("锚定结果应可通过机械校验: ok=%v err=%v", ok, verr)
	}
	if !strings.Contains(note, "1/1") {
		t.Fatalf("note 应含锚定统计: %q", note)
	}
}

func TestAnchorEvidenceRefsRejectsFabricatedSnippet(t *testing.T) {
	// 模型凭印象复述了一段不在响应里的输出——回执锚定正是要拦这个。
	ts := NewToolSet(nil, "worker")
	ts.SetTrafficBodyResolver(anchorBodyFn("HTTP/1.1 200 OK\r\n\r\n{\"user\":1}", nil))
	_, _, err := ts.anchorEvidenceRefs(context.Background(),
		[]db.TrafficRef{{TrafficID: "t1", Role: "proof"}},
		map[string]string{"t1": "root:x:0:0:root:/root:/bin/bash"})
	if err == nil || !strings.Contains(err.Error(), "锚定被拒") {
		t.Fatalf("伪造片段必须拒绝上报, got %v", err)
	}
}

func TestAnchorEvidenceRefsRejectsMissingTraffic(t *testing.T) {
	// 引用了取不到的流量 ID：同样拒绝（引用不存在的证据比没有证据更糟）。
	ts := NewToolSet(nil, "worker")
	ts.SetTrafficBodyResolver(anchorBodyFn("", errors.New("not found")))
	_, _, err := ts.anchorEvidenceRefs(context.Background(),
		[]db.TrafficRef{{TrafficID: "ghost", Role: "proof"}},
		map[string]string{"ghost": "anything"})
	if err == nil || !strings.Contains(err.Error(), "取响应原文出错") {
		t.Fatalf("取体失败应拒绝, got %v", err)
	}
}

func TestAnchorEvidenceRefsNilResolverNoop(t *testing.T) {
	// 未装配取体回调（独立运行/测试）：不锚定、不报错——渐进增强而不是新门槛。
	ts := NewToolSet(nil, "worker")
	refs := []db.TrafficRef{{TrafficID: "t1", Role: "proof"}}
	out, note, err := ts.anchorEvidenceRefs(context.Background(), refs, map[string]string{"t1": "x"})
	if err != nil || note != "" || out[0].Anchored() {
		t.Fatalf("nil 回调应原样返回: err=%v note=%q anchored=%v", err, note, out[0].Anchored())
	}
	// 装配了回调但没提供 slice_text 的引用：同样保持未锚定（历史工作流不受影响）。
	ts.SetTrafficBodyResolver(anchorBodyFn("body", nil))
	out, note, err = ts.anchorEvidenceRefs(context.Background(), refs, nil)
	if err != nil || note != "" || out[0].Anchored() {
		t.Fatalf("无 slice_text 应保持未锚定: err=%v note=%q", err, note)
	}
}
