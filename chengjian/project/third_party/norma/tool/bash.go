package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/norma/procguard"

	"github.com/Autumn-27/norma/permission"
)

// catastrophicBash matches a small set of irreversibly destructive commands
// that are hard-denied regardless of mode (NFR-10). This is a safety floor, not
// a full sandbox — hosts should still gate Bash via CanUseTool.
var catastrophicBash = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rf][a-zA-Z]*\s+(-[a-zA-Z]+\s+)*/(\s|$)`), // rm -rf /
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;`),                               // fork bomb
	regexp.MustCompile(`\bmkfs\.\w+\s+/dev/`),                                                      // format a device
	regexp.MustCompile(`\bdd\b[^\n]*\bof=/dev/(sd|nvme|hd)`),                                       // overwrite a disk
}

// NewBash builds the Bash tool: run a shell command with a timeout, optionally in
// the background.
func NewBash() CoreTool {
	props := map[string]any{
		"command":    map[string]any{"type": "string", "description": "The shell command to execute."},
		"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in ms (default 120000, max 600000)."},
	}
	desc := "Executes a shell command in the working directory and returns combined stdout and stderr. Use for running tests, builds, package managers, and git. Prefer the dedicated file tools for file work."
	prompt := ExecutionEnvironment() + "\nFor loops, parsing or HTTP logic prefer a saved short script over nested shell quoting. Bound concurrency (normally <=16), queue length and response reads; do not submit an entire large range to Executor.map without backpressure. timeout_ms is a HARD execution deadline, including explicitly backgrounded commands. Avoid interactive input."
	if !BackgroundTasksDisabled() {
		props["run_in_background"] = map[string]any{
			"type":        "boolean",
			"description": "Explicitly run in the background; read with TaskOutput/Read. The hard timeout still applies. Foreground commands never silently become background work.",
		}
		desc += " Set run_in_background to return immediately and read the output later."
	}
	return Build(Spec{
		Name:        "Bash",
		Description: desc,
		Prompt:      prompt,
		Schema: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []any{"command"},
		},
		Permissions: bashPermissions,
		Run:         runBash,
	})
}

func bashPermissions(_ context.Context, input json.RawMessage, _ permission.Context) permission.Decision {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	for _, re := range catastrophicBash {
		if re.MatchString(in.Command) {
			return permission.Denied("denied: command appears irreversibly destructive and is blocked by the safety floor")
		}
	}
	return permission.AskUser("run shell command: " + in.Command)
}

func runBash(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		Command         string `json:"command"`
		TimeoutMS       int    `json:"timeout_ms"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return Errorf("Error: command is required"), nil
	}
	if err := validateShellSyntax(in.Command); err != nil {
		return Errorf(err.Error()), nil
	}
	timeout := 120 * time.Second
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		if timeout > 600*time.Second {
			timeout = 600 * time.Second
		}
	}

	// Background-capable path: requires a task manager on the context.
	if tc != nil && tc.Tasks != nil && !BackgroundTasksDisabled() {
		return runBashManaged(ctx, tc, in.Command, timeout, in.RunInBackground)
	}
	// Fallback: synchronous execution (unchanged legacy behavior).
	return runBashSync(ctx, tc, in.Command, timeout)
}

// runBashManaged runs the command through the task manager, supporting explicit
// background launch and hard execution deadlines.
func runBashManaged(ctx context.Context, tc *ToolContext, command string, timeout time.Duration, explicitBG bool) (Result, error) {
	spec := SpawnSpec{Command: command, Kind: KindBash, WorkingDir: tc.WorkingDir, Description: short(command), Env: tc.Env, HardTimeout: timeout}

	if explicitBG {
		t, err := tc.Tasks.Spawn(spec)
		if err != nil {
			return Errorf("Error: failed to start background command: " + err.Error()), nil
		}
		tc.Tasks.Background(t.ID)
		return Text(fmt.Sprintf(
			"Running in background as %s. Output file: %s\nUse TaskOutput{task_id:%q} or Read to read its output; you are notified when it completes.",
			t.ID, t.OutputPath, t.ID)), nil
	}

	t, finished, output, err := tc.Tasks.RunForeground(ctx, spec, timeout)
	if err != nil {
		if t != nil {
			var keepErr error
			output, keepErr = retainTaskOutput(t.OutputPath, tc)
			if keepErr == nil {
				tc.Tasks.Forget(t.ID)
			} else {
				output += "\noutput_preservation_failed: original retained at " + t.OutputPath
			}
		}
		return Errorf("Error: " + err.Error() + "\n" + output), nil
	}
	if finished {
		info, _ := tc.Tasks.Get(t.ID)
		output, err = retainTaskOutput(t.OutputPath, tc)
		if err != nil {
			return Errorf("output_preservation_failed: " + err.Error() + "; original output: " + t.OutputPath), nil
		}
		tc.Tasks.Forget(t.ID)
		if info.Status != TaskCompleted {
			if strings.TrimSpace(output) == "" {
				output = "(no output)"
			}
			return Errorf(fmt.Sprintf("%s\n\n[exit code %d reason=%s peak_job_memory=%d]", output, info.ExitCode, info.Reason, info.PeakMemory)), nil
		}
		if strings.TrimSpace(output) == "" {
			output = "(command produced no output)"
		}
		return Text(output), nil
	}

	// A wait timeout is a hard stop, never implicit permission to keep running.
	out, _ := tc.Tasks.Output(t.ID, 0)
	tc.Tasks.Kill(t.ID)
	_, _ = tc.Tasks.Wait(context.Background(), t.ID, 5*time.Second)
	if kept, err := retainTaskOutput(t.OutputPath, tc); err == nil {
		out = kept
		tc.Tasks.Forget(t.ID)
	} else {
		out += "\noutput_preservation_failed: original retained at " + t.OutputPath
	}
	return Errorf(out + fmt.Sprintf("\n\n[command timed out after %s; process tree terminated]", timeout)), nil
}

// runBashSync is the original blocking implementation, used when no task manager
// is available (background execution disabled).
func runBashSync(ctx context.Context, tc *ToolContext, command string, timeout time.Duration) (Result, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flags := shellCmd()
	cmd := exec.CommandContext(cctx, shell, append(flags, command)...)
	if tc != nil {
		cmd.Dir = tc.WorkingDir
		if env := withEnv(tc.Env); env != nil {
			cmd.Env = env
		}
	}
	var buf procguard.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := procguard.Run(cctx, cmd)

	out := Capture(tc, string(buf.Bytes()))
	if buf.Overflow {
		return Errorf(out + "\n[output_limit_exceeded: execution output was incomplete]"), nil
	}
	if cctx.Err() == context.DeadlineExceeded {
		return Errorf(out + fmt.Sprintf("\n\n[command timed out after %s]", timeout)), nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if out == "" {
				out = "(no output)"
			}
			return Errorf(fmt.Sprintf("%s\n\n[exit code %d]", out, ee.ExitCode())), nil
		}
		return Errorf(fmt.Sprintf("%s\n\n[failed to run: %v]", out, err)), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(command produced no output)"
	}
	return Text(out), nil
}
