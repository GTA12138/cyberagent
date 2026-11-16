"use client";

import type { LucideIcon } from "lucide-react";

export type OverviewTone = "neutral" | "blue" | "green" | "amber" | "red";
export interface OverviewMetric {
  label: string;
  value: number | string;
  icon?: LucideIcon;
  tone?: OverviewTone;
}
export interface OverviewSegment {
  label: string;
  value: number;
  tone?: OverviewTone;
  active?: boolean;
  onSelect?: () => void;
}

export function overviewGroups<T>(items: readonly T[], key: (item: T) => string): OverviewSegment[] {
  const counts = new Map<string, number>();
  for (const item of items) {
    const name = key(item) || "未标识";
    counts.set(name, (counts.get(name) ?? 0) + 1);
  }
  const rows = [...counts].sort((a, b) => b[1] - a[1]);
  const visible = rows.slice(0, 5).map(([label, value], index) => ({
    label,
    value,
    tone: (["blue", "green", "neutral", "amber", "neutral"] as const)[index],
  }));
  if (rows.length > 5)
    visible.push({ label: "其他", value: rows.slice(5).reduce((sum, [, value]) => sum + value, 0), tone: "neutral" });
  return visible;
}

/** All values come from the owning page's existing response, never a second polling loop. */
export function ModuleOverview({
  label,
  metrics,
  segments = [],
  loading = false,
  error,
  stale = false,
}: {
  label: string;
  metrics: OverviewMetric[];
  segments?: OverviewSegment[];
  loading?: boolean;
  error?: string;
  stale?: boolean;
}) {
  const total = segments.reduce((sum, segment) => sum + Math.max(0, segment.value), 0);
  const unavailable = loading ? true : Boolean(error && !stale);
  return (
    <section
      className="console-overview"
      aria-label={label}
      aria-busy={loading && !error}
      data-console-overview
      data-stale={Boolean(error && stale)}
    >
      {error && (
        <p className="console-overview-notice" role="status">
          {error}
          {stale ? "；显示上次成功快照，可能不符合当前筛选。" : "；尚无可用统计。"}
        </p>
      )}
      <dl className="console-metrics">
        {metrics.map(({ label: name, value, icon: Icon, tone = "neutral" }) => (
          <div key={name} data-tone={tone}>
            <dt>
              {Icon && <Icon size={15} aria-hidden="true" />}
              {name}
            </dt>
            <dd>{unavailable ? "—" : typeof value === "number" ? value.toLocaleString() : value}</dd>
          </div>
        ))}
      </dl>
      {segments.length > 0 && (
        <div className="console-distribution">
          <div className="console-distribution-heading">
            <span>{label}</span>
            <small>{error ? "读取失败" : loading ? "读取中" : total > 0 ? "" : "暂无记录"}</small>
          </div>
          <div className="console-spectrum" aria-hidden="true">
            {!unavailable &&
              segments
                .filter((segment) => segment.value > 0)
                .map((segment) => (
                  <i key={segment.label} data-tone={segment.tone ?? "neutral"} style={{ flexGrow: segment.value }} />
                ))}
          </div>
          <div className="console-spectrum-legend">
            {segments.map((segment) =>
              segment.onSelect ? (
                <button
                  key={segment.label}
                  type="button"
                  data-tone={segment.tone ?? "neutral"}
                  aria-pressed={segment.active ?? false}
                  onClick={segment.onSelect}
                  disabled={unavailable}
                >
                  <i aria-hidden="true" />
                  {segment.label}
                  <b>{unavailable ? "—" : segment.value.toLocaleString()}</b>
                </button>
              ) : (
                <span key={segment.label} data-tone={segment.tone ?? "neutral"}>
                  <i aria-hidden="true" />
                  {segment.label}
                  <b>{unavailable ? "—" : segment.value.toLocaleString()}</b>
                </span>
              ),
            )}
          </div>
        </div>
      )}
    </section>
  );
}
