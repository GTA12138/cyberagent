package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"chengjian.local/chengjian/db"
)

func TestAssembleBlocksSkipsEmptyAndKeepsFirstHeaderless(t *testing.T) {
	got := assembleBlocks(
		PromptBlock{ID: BlockStableRole, Body: "你是 worker。"},
		PromptBlock{ID: BlockCriteria, Body: "  "}, // 空块必须被跳过
		PromptBlock{ID: BlockStopContract, Title: titleOf(BlockStopContract), Body: "写完就停。"},
	)
	if !strings.HasPrefix(got, "你是 worker。") {
		t.Fatalf("第一块不画标题头（正文自带开头），got %q", got)
	}
	if strings.Count(got, "【输出与停止合同】") != 1 {
		t.Fatalf("停止合同块应恰好出现一次，got %q", got)
	}
	if strings.Contains(got, "【本次场景判据】") {
		t.Fatal("空块不应渲染出标题")
	}
}

func TestWorkerSystemHasFiveBlockStructure(t *testing.T) {
	sys := workerSystem(t.Context(), "", false, "/data", "/data", false)
	// 块 1（职责）没有标题头，其余按顺序出现且各一次。
	order := []string{"【本次场景判据】", "【输出与停止合同】"}
	prev := -1
	for _, title := range order {
		if n := strings.Count(sys, title); n != 1 {
			t.Fatalf("%s 应恰好出现一次，实际 %d 次", title, n)
		}
		idx := strings.Index(sys, title)
		if idx <= prev {
			t.Fatalf("%s 的位置不对：块顺序必须是 职责 → 判据 → 停止合同", title)
		}
		prev = idx
	}
	// 判据块里的核心纪律不能被拆块过程丢掉。
	for _, want := range []string{"反 AI 蜜罐红线"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("判据块缺 %q", want)
		}
	}
}

func TestRenderIntentTaskCarriesCriteriaAsProse(t *testing.T) {
	n := &db.Node{
		ID: 1,
		Payload: json.RawMessage(`{"summary":"验证 SMB 能否横向",` +
			`"prerequisites":["session","credential:smb"],` +
			`"done_when":["fact:横向成功","command"]}`),
	}
	got := renderIntentTask(n)
	if !strings.Contains(got, "【当前任务】") {
		t.Fatalf("意图块应带块标题，got %q", got)
	}
	if !strings.Contains(got, "开工前应具备") || !strings.Contains(got, "credential:smb") {
		t.Fatal("前置条件必须以人话渲染，不能只留在原始 JSON 里")
	}
	if !strings.Contains(got, "收工前自查") || !strings.Contains(got, "fact:横向成功") {
		t.Fatal("完成判据必须以人话渲染——模型漏读判据就会以为做完了")
	}
}

func TestTitleOfUnknownReturnsID(t *testing.T) {
	if got := titleOf(PromptBlockID("nope")); got != "nope" {
		t.Fatalf("未知块应回退成 id，got %q", got)
	}
}
