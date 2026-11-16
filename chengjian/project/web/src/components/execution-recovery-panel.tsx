"use client";
import * as React from "react";
import { http } from "@/lib/api";
import { parseExecutionAttempts, type ExecutionAttempt } from "@/lib/execution-contract";
import { startSerialPoll } from "@/lib/serial-poller";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Textarea } from "@/components/ui/textarea";

export function ExecutionRecoveryPanel({ taskId }: { taskId: string }) {
  const [items, setItems] = React.useState<ExecutionAttempt[]>([]);
  const [error, setError] = React.useState("");
  const [busy, setBusy] = React.useState(false);
  const [assessment, setAssessment] = React.useState<Record<number, string>>({});
  const poll = React.useRef<ReturnType<typeof startSerialPoll> | null>(null);
  React.useEffect(() => {
    setItems([]);
    setError("");
    setAssessment({});
    const current = startSerialPoll({
      fetch: (signal) =>
        http<unknown>(`/tasks/${encodeURIComponent(taskId)}/execution-attempts`, { signal }).then(
          parseExecutionAttempts,
        ),
      receive: (rows) => {
        setItems(rows);
        setError("");
      },
      error: (err) => setError(`执行核对记录读取失败：${(err as Error).message}`),
      interval: 10000,
    });
    poll.current = current;
    return () => {
      current.stop();
      poll.current = null;
    };
  }, [taskId]);
  const resolve = async (item: ExecutionAttempt, state: "completed" | "not_executed") => {
    if (!assessment[item.id]?.trim()) return;
    setBusy(true);
    try {
      await http(`/tasks/${encodeURIComponent(taskId)}/execution-attempts/${item.id}/resolve`, {
        method: "POST",
        body: JSON.stringify({ state, assessment: assessment[item.id].trim() }),
      });
      setItems((rows) => rows.filter((row) => row.id !== item.id));
      poll.current?.refresh();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };
  const unknown = items.filter((item) => item.state === "unknown");
  if (!unknown.length && !error) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle>执行结果待核对</CardTitle>
      </CardHeader>
      <CardContent className="space-y-4">
        <p className="text-sm text-muted-foreground">
          以下操作中断后结果尚未确认。请检查实际执行结果，填写核对依据。确认后，可在会话页手动继续对应意图。
        </p>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {unknown.map((item) => (
          <div key={item.id} className="space-y-2 rounded-md border p-3">
            <p className="text-sm">
              意图 #{item.intent_id || "—"} · {item.tool} · 记录 #{item.id}
            </p>
            {item.assessment && <p className="text-sm text-muted-foreground">{item.assessment}</p>}
            <Textarea
              aria-label={`记录 ${item.id} 的核对依据`}
              value={assessment[item.id] ?? ""}
              onChange={(event) => setAssessment((prev) => ({ ...prev, [item.id]: event.target.value }))}
              placeholder="说明检查了什么、依据是什么"
            />
            <div className="flex flex-wrap gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={busy || !assessment[item.id]?.trim()}
                onClick={() => void resolve(item, "completed")}
              >
                已确认执行完成
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={busy || !assessment[item.id]?.trim()}
                onClick={() => void resolve(item, "not_executed")}
              >
                已确认未执行，可重试
              </Button>
            </div>
          </div>
        ))}
      </CardContent>
    </Card>
  );
}
