import type { Finding, Task } from "@/lib/types";

export const SEVERITIES = ["critical", "high", "medium", "low"] as const;
export const SEVERITY_META = {
  critical: { label: "严重", color: "#d93655" },
  high: { label: "高危", color: "#e47a36" },
  medium: { label: "中危", color: "#c79822" },
  low: { label: "低危", color: "#6686aa" },
};

export function relativeTime(value?: string | number) {
  if (!value) return "—";
  const time = typeof value === "number" ? value * 1000 : Date.parse(value);
  if (!Number.isFinite(time)) return "—";
  const minutes = Math.max(0, Math.floor((Date.now() - time) / 60000));
  if (minutes < 1) return "刚刚";
  if (minutes < 60) return `${minutes} 分钟前`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)} 小时前`;
  return `${Math.floor(minutes / 1440)} 天前`;
}

export function taskLabel(task: Pick<Task, "id" | "name" | "description">) {
  if (task.name?.trim()) return task.name;
  const host = task.description.match(/https?:\/\/([^/\s，。]+)/)?.[1];
  return host ? `${host} 安全测试` : `渗透测试任务 #${task.id}`;
}

export function isOpenFinding(finding: Finding) {
  return !["fixed", "resolved", "false_positive", "ignored", "duplicate", "risk_accepted"].includes(finding.status);
}

export function prioritizeFindings(findings: Finding[], sort: "severity" | "time") {
  return [...findings].sort((a, b) => {
    if (sort === "severity") {
      const difference = SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity);
      if (difference) return difference;
    }
    return (Date.parse(b.ts) || 0) - (Date.parse(a.ts) || 0);
  });
}

function localDateKey(date: Date) {
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

export function findingTrend(findings: Finding[], days: number, now = new Date()) {
  const rows = Array.from({ length: days }, (_, index) => {
    const date = new Date(now);
    date.setDate(date.getDate() - days + index + 1);
    return { date: localDateKey(date), critical: 0, high: 0, medium: 0, low: 0 };
  });
  const byDate = new Map(rows.map((row) => [row.date, row]));
  for (const finding of findings) {
    const timestamp = new Date(finding.ts);
    if (!Number.isFinite(timestamp.getTime())) continue;
    const row = byDate.get(localDateKey(timestamp));
    if (row && SEVERITIES.includes(finding.severity)) row[finding.severity]++;
  }
  return rows;
}
