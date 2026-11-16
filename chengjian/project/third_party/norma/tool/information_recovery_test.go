package tool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCHENGJIANReadBytePagesRecoverLongLineAndBinary(t *testing.T) {
	dir := t.TempDir()
	body := strings.Repeat("长行内容", 4000) + "TAIL_MARKER"
	path := filepath.Join(dir, "one-line.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var recovered []byte
	for offset := 0; offset < len(body); {
		in, _ := json.Marshal(map[string]any{"file_path": path, "byte_offset": offset, "byte_limit": 4096, "encoding": "base64"})
		r, err := NewRead().Call(context.Background(), in, &ToolContext{WorkingDir: dir})
		if err != nil || r.IsError {
			t.Fatalf("page failed: %v %v", r, err)
		}
		_, encoded, ok := strings.Cut(r.Flatten(), "\n")
		if !ok {
			t.Fatal("page has no body")
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(data) == 0 {
			t.Fatalf("invalid page: %v", err)
		}
		recovered = append(recovered, data...)
		offset += len(data)
	}
	if string(recovered) != body {
		t.Fatal("long-line/UTF-8 bytes were lost")
	}
}

func TestCHENGJIANExplicitBackgroundHardDeadline(t *testing.T) {
	m := newTestManager(t)
	command := "while true; do sleep 1; done"
	if runtime.GOOS == "windows" {
		command = "while ($true) { Start-Sleep -Seconds 1 }"
	}
	r := runTC(t, NewBash(), &ToolContext{Tasks: m}, map[string]any{"command": command, "timeout_ms": 500, "run_in_background": true})
	if r.IsError {
		t.Fatal(r.Flatten())
	}
	tasks := m.List()
	if len(tasks) != 1 {
		t.Fatal("missing explicit background task")
	}
	info, done := m.Wait(context.Background(), tasks[0].ID, 5*time.Second)
	if !done || info.Status != TaskFailed || info.Reason != "hard_timeout" {
		t.Fatalf("background escaped deadline: %+v done=%t", info, done)
	}
}

func TestCHENGJIANTaskOutputBytePagesPreserveSplitUTF8(t *testing.T) {
	m := newTestManager(t)
	command := "printf 'abc'"
	if runtime.GOOS == "windows" {
		command = "Write-Output 'abc'"
	}
	task, err := m.Spawn(SpawnSpec{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	if _, done := m.Wait(context.Background(), task.ID, 5*time.Second); !done {
		t.Fatal("fixture did not exit")
	}
	body := []byte("a中文尾\x00end")
	if err := os.WriteFile(task.OutputPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var recovered []byte
	for offset := 0; offset < len(body); {
		r := runTC(t, NewTaskOutput(), &ToolContext{Tasks: m}, map[string]any{"task_id": task.ID, "offset": offset, "max_bytes": 2})
		lines := strings.SplitN(r.Flatten(), "\n", 4)
		if len(lines) != 4 || r.IsError {
			t.Fatal(r.Flatten())
		}
		data := []byte(lines[3])
		if strings.Contains(lines[2], "encoding=base64") {
			data, err = base64.StdEncoding.DecodeString(lines[3])
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(data) == 0 {
			t.Fatal("cursor did not advance")
		}
		recovered = append(recovered, data...)
		offset += len(data)
	}
	if string(recovered) != string(body) {
		t.Fatal("TaskOutput lost UTF-8/binary bytes")
	}
}

func TestCHENGJIANGrepDoesNotReportMissingInfoAsNoMatch(t *testing.T) {
	t.Setenv("NORMA_DISABLE_RIPGREP", "1")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.txt"), []byte("needle in accessible file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.txt"), []byte(strings.Repeat("x", (1<<20)+1)+"needle in tail"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runTC(t, NewGrep(), &ToolContext{WorkingDir: dir}, map[string]any{"pattern": "needle", "output_mode": "content"})
	if !r.IsError || !strings.Contains(r.Flatten(), "search_incomplete") || !strings.Contains(r.Flatten(), "small.txt") || !strings.Contains(r.Flatten(), "large.txt") {
		t.Fatalf("search silently lost evidence: %s", r.Flatten())
	}
}

func TestCHENGJIANManagedOutputHasHardDiskLimit(t *testing.T) {
	m := newTestManager(t)
	command := "head -c 54525952 /dev/zero"
	if runtime.GOOS == "windows" {
		command = "[Console]::OpenStandardOutput().Write((New-Object byte[] (52*1024*1024)),0,(52*1024*1024))"
	}
	task, err := m.Spawn(SpawnSpec{Command: command, HardTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	info, done := m.Wait(context.Background(), task.ID, 20*time.Second)
	if !done || info.Status != TaskFailed || info.Reason != "output_limit_exceeded" {
		t.Fatalf("output quota not enforced: %+v done=%t", info, done)
	}
	stat, err := os.Stat(task.OutputPath)
	if err != nil || stat.Size() > maxTaskOutputBytes {
		t.Fatal("output grew beyond disk budget", err)
	}
}

func TestCHENGJIANForegroundOutputSurvivesForget(t *testing.T) {
	dir := t.TempDir()
	m := newTestManager(t)
	tc := &ToolContext{Tasks: m, WorkingDir: dir, OutputDir: filepath.Join(dir, "output"), MaxOutputChars: 1000}
	command := "printf 'HEAD'; head -c 60000 /dev/zero | tr '\\0' x; printf 'TAIL'"
	if runtime.GOOS == "windows" {
		command = "[Console]::Write('HEAD' + ('x' * 60000) + 'TAIL')"
	}
	r := runTC(t, NewBash(), tc, map[string]any{"command": command})
	if r.IsError || !strings.Contains(r.Flatten(), "full_output=") {
		t.Fatalf("output not preserved: %s", r.Flatten())
	}
	files, err := filepath.Glob(filepath.Join(tc.OutputDir, "command-*.txt"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing durable output")
	}
	data, err := os.ReadFile(files[0])
	if err != nil || !strings.HasPrefix(string(data), "HEAD") || !strings.HasSuffix(string(data), "TAIL") || len(data) < 60008 {
		t.Fatal("head or tail discarded")
	}
	if len(m.List()) != 0 {
		t.Fatal("foreground task not forgotten")
	}
}

func TestCHENGJIANWebFetchRawPreservesHiddenAndStaticText(t *testing.T) {
	const source = `/*SECRET*/ const hiddenEndpoint='/internal-info';`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		fmt.Fprint(w, source)
	}))
	defer srv.Close()
	tc := &ToolContext{WorkingDir: t.TempDir()}
	in, _ := json.Marshal(map[string]any{"url": srv.URL + "/app.js", "raw": true})
	r, err := NewWebFetch(WebFetchConfig{}).Call(context.Background(), in, tc)
	if err != nil || r.IsError || !strings.Contains(r.Flatten(), source) || !strings.Contains(r.Flatten(), "body_file=") {
		t.Fatalf("raw resource inaccessible: %v %v", r, err)
	}
}

func TestCHENGJIANWindowsSyntaxGuardSkipsQuotedProgramText(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows syntax")
	}
	for _, command := range []string{"echo x && echo y", "echo x || echo y", "cat << EOF", "curl.exe -o /dev/null https://fixture.invalid"} {
		if validateShellSyntax(command) == nil {
			t.Errorf("accepted incompatible shell syntax: %s", command)
		}
	}
	if err := validateShellSyntax(`python -c "print('x && y')"`); err != nil {
		t.Fatal("program literal incorrectly rejected")
	}
}
