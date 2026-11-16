import type { ExecutionAttempt } from "./api-contracts.generated";
export type { ExecutionAttempt } from "./api-contracts.generated";

export function parseExecutionAttempts(value: unknown): ExecutionAttempt[] {
  if (!value || typeof value !== "object" || !Array.isArray((value as { items?: unknown }).items)) {
    throw new Error("执行记录响应格式不匹配");
  }
  return (value as { items: unknown[] }).items.map((item) => {
    const row = item as ExecutionAttempt;
    if (
      !row ||
      !Number.isSafeInteger(row.id) ||
      row.id <= 0 ||
      !Number.isSafeInteger(row.task_id) ||
      row.task_id <= 0 ||
      !Number.isSafeInteger(row.intent_id) ||
      row.intent_id < 0 ||
      typeof row.tool !== "string" ||
      !["started", "unknown"].includes(row.state) ||
      typeof row.assessment !== "string"
    ) {
      throw new Error("执行记录字段与接口契约不匹配");
    }
    return row;
  });
}
