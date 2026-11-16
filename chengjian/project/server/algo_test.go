// algo_test.go 预置算法库的执行契约与命中提示测试（本地 python3，无外联）。
package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 契约测试：stdin JSON → stdout JSON；容忍杂散日志行（取最后一个 { 开头的行）；
// 超时/非 JSON 输出报可操作错误。
func TestRunAlgorithmScriptContract(t *testing.T) {
	script := `
import sys, json
params = json.loads(sys.stdin.readline())
print("debug: got", params.get("ts"))   # 杂散日志行必须被容忍
result = {"sign": "h-" + str(params["ts"]) + "-" + params["body"][:4], "ok": True}
print(json.dumps(result))
`
	out, err := runAlgorithmScript(context.Background(), script, json.RawMessage(`{"ts":1698,"body":"abcdef"}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sign string `json:"sign"`
		OK   bool   `json:"ok"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出应为 JSON: %v (%s)", err, out)
	}
	if got.Sign != "h-1698-abcd" || !got.OK {
		t.Fatalf("计算结果错误: %+v", got)
	}
}

func TestRunAlgorithmScriptRejectsNonJSON(t *testing.T) {
	if _, err := runAlgorithmScript(context.Background(), `print("hello")`, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "没有输出 JSON") {
		t.Fatalf("非 JSON 输出应报契约错误, got %v", err)
	}
	// 死循环用 2s 短超时（Windows 上进程收割慢，15s 全额会拖慢测试）；
	// 生产路径同样受有界宽限保护——超时必须按时返回而不是挂住平台。
	if _, err := runAlgorithmScriptWithTimeout(context.Background(), `while True: pass`, json.RawMessage(`{}`), 2*time.Second); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("死循环应在超时后按时返回, got %v", err)
	}
	if _, err := runAlgorithmScript(context.Background(), `raise SystemExit(2)`, json.RawMessage(`{}`)); err == nil {
		t.Fatal("脚本崩溃应报错")
	}
}

// 滑块形态的最小实现：纯计算生成轨迹参数（真实场景是把逆向出的轨迹/加密
// 逻辑放进来，这里验证契约管道对 slider 类同样成立）。
func TestRunAlgorithmSliderShape(t *testing.T) {
	script := `
import sys, json
p = json.loads(sys.stdin.readline())
gap = int(p["gap"])
points = [[0,0,0],[gap//2,1,120],[gap,0,240]]
print(json.dumps({"trajectory": points, "sig": f"s{gap}"}))
`
	out, err := runAlgorithmScript(context.Background(), script, json.RawMessage(`{"gap":87}`))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Sig        string  `json:"sig"`
		Trajectory [][]int `json:"trajectory"`
	}
	if err := json.Unmarshal(out, &got); err != nil || got.Sig != "s87" || len(got.Trajectory) != 3 {
		t.Fatalf("滑块轨迹计算错误: %s (%v)", out, err)
	}
}
