package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var environmentOnce sync.Once
var environmentNote string

// ExecutionEnvironment is code-owned and attached after editable DB metadata.
func ExecutionEnvironment() string {
	environmentOnce.Do(func() {
		shell, _ := shellCmd()
		path, _ := exec.LookPath(shell)
		environmentNote = fmt.Sprintf("[实际执行环境] OS=%s/%s; shell=%s。工具虽然名为 Bash，实际以此 shell 执行。", runtime.GOOS, runtime.GOARCH, path)
		if runtime.GOOS == "windows" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", "$PSVersionTable.PSVersion.ToString()")
			setDetached(cmd)
			if out, err := cmd.Output(); err == nil {
				environmentNote += " PowerShell版本=" + strings.TrimSpace(string(out)) + "。"
			}
			environmentNote += " 使用 PowerShell 语法；禁止 Bash 的 &&、||、heredoc、/dev/null。用分开的调用或 ; 和 $LASTEXITCODE 判断；丢弃输出用 $null。外部 curl 用 curl.exe，环境变量用 $env:NAME（如 $env:CHENGJIAN_UA）。Python/PHP 代码保存成文件再运行，避免 shell 对 $、引号进行插值；不要假设 head/which/mysql 等已安装。"
		} else {
			environmentNote += " 使用 Bash 语法。"
		}
		for _, name := range []string{"python3", "python", "node", "curl.exe", "git", "php", "mysql", "nmap"} {
			if p, err := exec.LookPath(name); err == nil {
				environmentNote += "\n" + name + "=" + p
			}
		}
		environmentNote += "\n命令有硬超时和资源配额；失败不等于目标无资源。输出超过展示预算会保留原文并给出 Read 续读位置。"
	})
	return environmentNote
}

func validateShellSyntax(command string) error {
	if runtime.GOOS != "windows" {
		return nil
	}
	// Ignore quoted program arguments, including Python/SQL strings containing
	// these characters. Only reject operators at the actual shell level.
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if c == '`' {
			i++
			continue
		}
		if quote != 0 {
			if c == quote {
				if quote == '\'' && i+1 < len(command) && command[i+1] == '\'' {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if strings.HasPrefix(command[i:], "&&") || strings.HasPrefix(command[i:], "||") || strings.HasPrefix(command[i:], "<<") || strings.HasPrefix(command[i:], "/dev/null") {
			return fmt.Errorf("shell_syntax_mismatch: 实际使用 Windows PowerShell；请改用 PowerShell 语法、$null、单独命令及脚本文件，不能执行 Bash 操作符/路径。此命令未执行。")
		}
	}
	return nil
}

// retainTaskOutput preserves the full bounded file BEFORE Manager.Forget removes
// it. Reading just its tail then forgetting previously destroyed the beginning.
func retainTaskOutput(path string, tc *ToolContext) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	budget := maxOut(tc)
	if info.Size() <= int64(budget) {
		b, err := io.ReadAll(io.LimitReader(f, int64(budget)))
		return string(b), err
	}
	dir := ""
	if tc != nil {
		dir = tc.OutputDir
		if dir == "" && tc.WorkingDir != "" {
			dir = filepath.Join(tc.WorkingDir, "cmd-output")
		}
	}
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "chengjian-tool-output")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	full, err := os.CreateTemp(dir, "command-*.txt")
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(full, io.LimitReader(f, maxTaskOutputBytes+1))
	closeErr := full.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	_, _ = f.Seek(0, io.SeekStart)
	head, err := io.ReadAll(io.LimitReader(f, int64(max(1, budget-1200))))
	if err != nil {
		return "", err
	}
	note := fmt.Sprintf("\n<persisted-output>full_output=%s bytes=%d; Read(file_path=该路径, byte_offset=%d, byte_limit=8192) 可续读；也可从 byte_offset=0 开始。</persisted-output>", full.Name(), n, len(head))
	if n > maxTaskOutputBytes {
		note += "\n[output_limit_exceeded: source exceeds the hard retention limit; incomplete=true]"
	}
	return string(head) + note, nil
}
