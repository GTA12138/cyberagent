package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"
	"unicode/utf8"
)

// NewTaskOutput builds the TaskOutput tool: read the captured output of a
// background task (from Bash run_in_background or Monitor), optionally blocking
// until it finishes.
func NewTaskOutput() CoreTool {
	return Build(Spec{
		Name:        "TaskOutput",
		Description: "Reads the output of a background task started by Bash (run_in_background) or Monitor. You can also Read the task's output-file path directly. Set block=true to wait for the task to finish first.",
		Prompt:      "Provide the task_id. With block=true the call waits until the task reaches a terminal state (up to timeout_ms) before returning its output.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id":    map[string]any{"type": "string", "description": "The task id (e.g. task_1)."},
				"max_bytes":  map[string]any{"type": "integer", "description": "Max bytes of output to return from the tail (default 30000)."},
				"offset":     map[string]any{"type": "integer", "minimum": 0, "description": "Read a byte page from the start/next_offset instead of returning only the tail."},
				"block":      map[string]any{"type": "boolean", "description": "Wait until the task finishes before returning (default false)."},
				"timeout_ms": map[string]any{"type": "integer", "description": "Max wait when block=true (default 30000)."},
			},
			"required": []any{"task_id"},
		},
		ReadOnly:    always,
		Concurrent:  always,
		Permissions: allowReadOnly,
		Run:         runTaskOutput,
	})
}

func runTaskOutput(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		TaskID    string `json:"task_id"`
		MaxBytes  int    `json:"max_bytes"`
		Block     bool   `json:"block"`
		TimeoutMS int    `json:"timeout_ms"`
		Offset    *int64 `json:"offset"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if tc == nil || tc.Tasks == nil {
		return Errorf("Error: background tasks are not enabled"), nil
	}
	if in.TaskID == "" {
		return Errorf("Error: task_id is required"), nil
	}
	info, ok := tc.Tasks.Get(in.TaskID)
	if !ok {
		return Errorf(fmt.Sprintf("Error: unknown task %q", in.TaskID)), nil
	}
	if in.Block && !info.Status.Terminal() {
		timeout := 30 * time.Second
		if in.TimeoutMS > 0 {
			timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		}
		info, _ = tc.Tasks.Wait(ctx, in.TaskID, timeout)
	}
	if in.Offset != nil && *in.Offset < 0 {
		return Errorf("offset must be nonnegative"), nil
	}
	if in.MaxBytes <= 0 {
		in.MaxBytes = maxOut(tc)
	}
	in.MaxBytes = min(in.MaxBytes, max(1, (maxOut(tc)-1200)*3/4))
	var out string
	var err error
	if in.Offset != nil {
		f, e := os.Open(info.OutputPath)
		if e != nil {
			return Errorf(e.Error()), nil
		}
		defer f.Close()
		stat, e := f.Stat()
		if e != nil {
			return Errorf(e.Error()), nil
		}
		start := min(*in.Offset, stat.Size())
		buf := make([]byte, min(int64(in.MaxBytes), stat.Size()-start))
		n, e := f.ReadAt(buf, start)
		if e != nil && e != io.EOF {
			return Errorf(e.Error()), nil
		}
		encoding, body := "text", string(buf[:n])
		if isBinary(buf[:n]) || !utf8.Valid(buf[:n]) {
			encoding, body = "base64", base64.StdEncoding.EncodeToString(buf[:n])
		}
		out = fmt.Sprintf("[offset=%d next_offset=%d total_bytes=%d complete=%t encoding=%s]\n%s", start, start+int64(n), stat.Size(), start+int64(n) >= stat.Size(), encoding, body)
	} else {
		out, err = tc.Tasks.Output(in.TaskID, in.MaxBytes)
	}
	if err != nil {
		return Errorf("Error: " + err.Error()), nil
	}
	header := fmt.Sprintf("[task %s | %s", info.ID, info.Status)
	if info.Status == TaskCompleted || info.Status == TaskFailed {
		header += fmt.Sprintf(" | exit %d", info.ExitCode)
	}
	header += fmt.Sprintf(" | reason=%s | peak_job_memory=%d]\n[full_output=%s; TaskOutput(offset=0) 或 Read(byte_offset=0) 可从头完整分段读取]\n", info.Reason, info.PeakMemory, info.OutputPath)
	return Text(header + out), nil
}
