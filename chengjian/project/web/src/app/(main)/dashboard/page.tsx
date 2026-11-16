"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import Link from "next/link";

import {
  Activity,
  ArrowDown,
  ArrowRight,
  ArrowUp,
  ArrowUpRight,
  Cpu,
  HardDrive,
  Layers,
  Network,
  RefreshCw,
  ShieldCheck,
} from "lucide-react";

import { api } from "@/lib/api";
import {
  type DashboardOverview,
  type DashboardPoint,
  type DashboardUsage,
  groupFindings,
  level,
  usageTotals,
  utcDays,
} from "@/lib/dashboard";

import { useDashboardSource } from "./use-dashboard-source";
import "./dashboard.css";

const number = (value: number) => value.toLocaleString("en-US");
const compact = (value: number) =>
  Intl.NumberFormat("en-US", { notation: "compact", maximumFractionDigits: 1 }).format(value);
const bytes = (value: number) => {
  const units = ["B", "KB", "MB", "GB", "TB"];
  const i = value > 0 ? Math.min(4, Math.floor(Math.log(value) / Math.log(1024))) : 0;
  return `${(value / 1024 ** i).toFixed(i > 1 ? 1 : 0)} ${units[i]}`;
};
const pct = (value: number, total: number) => (total ? Math.min(100, (value / total) * 100) : 0);
const severityLabel: Record<string, string> = { critical: "严重", high: "高危", medium: "中危", low: "低危" };
const assetLabel: Record<string, string> = {
  endpoint: "接口",
  service: "服务",
  subdomain: "子域",
  ip: "IP",
  app: "应用",
  root_domain: "主域",
};
const statusLabel: Record<string, string> = {
  created: "已创建",
  queued: "排队中",
  running: "运行中",
  paused: "已暂停",
  done: "已完成",
  failed: "失败",
  timeout: "超时",
};
const loadTasks = () => api.tasks();
const loadTraffic = () => api.traffic(0, 1);

function Count({
  value,
  decimals = 0,
  short = false,
}: {
  value: number | null | undefined;
  decimals?: number;
  short?: boolean;
}) {
  const [display, setDisplay] = useState<number | null>(null);
  const previous = useRef<number | null>(null);
  useEffect(() => {
    if (value == null) {
      setDisplay(null);
      return;
    }
    const start = previous.current ?? value;
    previous.current = value;
    if (matchMedia("(prefers-reduced-motion: reduce)").matches || start === value) {
      setDisplay(value);
      return;
    }
    const since = performance.now();
    let frame = 0;
    const tick = (now: number) => {
      const progress = Math.min(1, (now - since) / 550);
      setDisplay(start + (value - start) * (1 - (1 - progress) ** 3));
      if (progress < 1) frame = requestAnimationFrame(tick);
    };
    frame = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(frame);
  }, [value]);
  return (
    <span title={value == null ? "等待采样" : number(value)}>
      {display == null
        ? "—"
        : short
          ? compact(Math.round(display))
          : display.toLocaleString("en-US", { minimumFractionDigits: decimals, maximumFractionDigits: decimals })}
    </span>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="dash-empty">{children}</p>;
}
function SourceState({ error, updated }: { error: boolean; updated: Date | null }) {
  return (
    <span className={`dash-source ${error ? "is-stale" : ""}`} role="status">
      {error ? (updated ? "更新中断 · 保留上次数据" : "暂不可用 · 正在重试") : updated ? "已同步" : "连接中"}
    </span>
  );
}
function SectionTitle({ label, title, children }: { label: string; title: string; children?: React.ReactNode }) {
  return (
    <div className="dash-section-title">
      <h2>
        <span className="dash-label">{label}</span>
        {title}
      </h2>
      <div>{children}</div>
    </div>
  );
}
function Spark({
  history,
  field,
  max = 100,
}: {
  history: DashboardPoint[];
  field: "cpu" | "memory" | "rx";
  max?: number;
}) {
  const actualMax = max > 0 ? max : Math.max(1, ...history.map((p) => p[field] ?? 0));
  return (
    <div
      className="dash-spark"
      role="img"
      aria-label={`最近 ${history.filter((p) => p[field] != null).length} 个真实采样点`}
    >
      {Array.from({ length: 60 }, (_, index) => {
        const point = history[index - 60 + history.length];
        const value = point?.[field];
        return (
          <span
            key={point?.at ?? `empty-${index}`}
            className={value == null ? "unmeasured" : ""}
            title={
              value == null
                ? "等待采样"
                : `${new Date(point.at).toLocaleTimeString()} · ${field === "rx" ? `${bytes(value)}/s` : `${value.toFixed(1)}%`}`
            }
          >
            <i
              style={{
                height:
                  value == null ? 0 : `${Math.max(2, Math.round((Math.min(1, value / actualMax) * 44) / 2) * 2)}px`,
              }}
            />
          </span>
        );
      })}
    </div>
  );
}
function Composition({ usage }: { usage: DashboardUsage }) {
  const total = usage.input + usage.output;
  const hit = Math.min(usage.input, usage.cache);
  const pieces = [
    { label: "缓存命中", value: hit, cls: "cache" },
    { label: "未命中输入", value: usage.input - hit, cls: "input" },
    { label: "输出", value: usage.output, cls: "output" },
  ];
  return (
    <>
      <div className="dash-composition" role="img" aria-label="Token 构成">
        {pieces.map((p) => (
          <i
            key={p.cls}
            className={p.cls}
            style={{ width: `${pct(p.value, total)}%` }}
            title={`${p.label} ${number(p.value)}`}
          />
        ))}
      </div>
      <div className="dash-legend">
        {pieces.map((p) => (
          <span key={p.cls}>
            <i className={p.cls} />
            {p.label}
            <b>{compact(p.value)}</b>
            <small>{pct(p.value, total).toFixed(1)}%</small>
          </span>
        ))}
      </div>
    </>
  );
}

function UsageCharts({ data }: { data: DashboardOverview }) {
  const days = utcDays(data.sampled_at, data.days);
  const daily = days.map((day) => ({ day, ...usageTotals(data.hourly.filter((row) => row.day === day)) }));
  const maxDay = Math.max(1, ...daily.map((p) => p.input + p.output));
  const matrixDays = days.slice(-7);
  const byHour = new Map(data.hourly.map((row) => [`${row.day}/${row.hour}`, row]));
  const maxInput = Math.max(1, ...data.hourly.filter((row) => matrixDays.includes(row.day)).map((p) => p.input));
  const maxOutput = Math.max(1, ...data.hourly.filter((row) => matrixDays.includes(row.day)).map((p) => p.output));
  const [selected, setSelected] = useState<string | null>(null);
  const selectedRow = selected ? byHour.get(selected) : null;
  const [selectedDay, setSelectedDay] = useState<string | null>(null);
  const dayUsage = daily.find((p) => p.day === selectedDay);
  const currentHour = new Date(data.sampled_at).getUTCHours();
  return (
    <div className="dash-usage-charts">
      <div className="dash-daily">
        <div className="dash-chart-caption">
          <span>每日用量</span>
          <span className="dash-label">{data.days} DAYS / UTC</span>
        </div>
        <div className="dash-daily-bars">
          {daily.map((row) => (
            <button
              type="button"
              key={row.day}
              aria-pressed={selectedDay === row.day}
              aria-label={`${row.day} 输入 ${row.input}，输出 ${row.output}`}
              title={`${row.day} · ${number(row.input + row.output)} tokens`}
              onClick={() => setSelectedDay(row.day)}
            >
              <span className="dash-day-stack" style={{ height: `${pct(row.input + row.output, maxDay)}%` }}>
                <i className="output" style={{ flex: row.output }} />
                <i className="input" style={{ flex: row.input }} />
              </span>
            </button>
          ))}
        </div>
        <div className="dash-chart-axis">
          <span>{days[0].slice(5)}</span>
          <span>{days.at(-1)?.slice(5)}</span>
        </div>
        <p className="dash-chart-readout" aria-live="polite">
          {dayUsage ? (
            <>
              {dayUsage.day} · 输入 {compact(dayUsage.input)} / 输出 {compact(dayUsage.output)}
            </>
          ) : (
            "点击柱形，查看当日用量"
          )}
        </p>
      </div>
      <div className="dash-hourly">
        <div className="dash-chart-caption">
          <span>逐小时输入 / 输出</span>
          <span className="dash-label">RECENT 7 DAYS / UTC</span>
        </div>
        <div className="dash-hour-scroll">
          <div className="dash-hour-grid">
            <div />
            {Array.from({ length: 24 }, (_, hour) => (
              <span className="dash-hour-label" key={hour}>
                {hour % 3 === 0 ? String(hour).padStart(2, "0") : ""}
              </span>
            ))}
            {matrixDays.map((day) => (
              <div className="dash-hour-row" key={day}>
                <span className="dash-hour-date">{day.slice(5)}</span>
                {Array.from({ length: 24 }, (_, hour) => {
                  const key = `${day}/${hour}`;
                  const row = byHour.get(key);
                  const future = day === days.at(-1) && hour > currentHour;
                  return (
                    <button
                      key={hour}
                      type="button"
                      className="dash-hour-cell"
                      disabled={future}
                      aria-pressed={selected === key}
                      aria-label={`${day} ${hour}:00 UTC，${future ? "尚未发生" : `输入 ${row?.input ?? 0}，输出 ${row?.output ?? 0}`}`}
                      title={
                        future
                          ? "尚未发生"
                          : `${day} ${hour}:00 UTC · 输入 ${number(row?.input ?? 0)} / 输出 ${number(row?.output ?? 0)}`
                      }
                      onClick={() => setSelected(key)}
                    >
                      <i className="input" style={{ height: `${pct(row?.input ?? 0, maxInput)}%` }} />
                      <i className="output" style={{ height: `${pct(row?.output ?? 0, maxOutput)}%` }} />
                    </button>
                  );
                })}
              </div>
            ))}
          </div>
        </div>
        <p className="dash-chart-readout" aria-live="polite">
          {selected ? (
            <>
              {selected.replace("/", " ")}:00 UTC · {number(selectedRow?.calls ?? 0)} 次调用 · 输入{" "}
              {compact(selectedRow?.input ?? 0)} / 输出 {compact(selectedRow?.output ?? 0)}
            </>
          ) : (
            "点击方格查看详情 · 绿色输入 / 橙色输出各用独立标尺"
          )}
        </p>
      </div>
    </div>
  );
}

function ActivityCalendar({ data }: { data: DashboardOverview }) {
  const days = utcDays(data.sampled_at, 365);
  const lookup = new Map(data.activity.map((p) => [p.day, p.count]));
  const max = Math.max(1, ...data.activity.map((p) => p.count));
  const total = data.activity.reduce((n, p) => n + p.count, 0);
  const [selected, setSelected] = useState<string | null>(null);
  const lead = new Date(days[0]).getUTCDay();
  return (
    <section className="dash-panel dash-calendar-panel">
      <SectionTitle label="ACTIVITY" title="执行活动">
        <Activity size={16} />
      </SectionTitle>
      <div className="dash-calendar-total">
        <Count value={total} short />
        <span>条活动 · 近 365 天</span>
      </div>
      <div className="dash-calendar-scroll">
        <div className="dash-calendar" style={{ gridTemplateColumns: `repeat(${Math.ceil((365 + lead) / 7)}, 1fr)` }}>
          {Array.from({ length: lead }, (_, i) => (
            <span className="dash-calendar-pad" key={`pad-${i}`} />
          ))}
          {days.map((day) => (
            <button
              type="button"
              key={day}
              data-level={level(lookup.get(day) ?? 0, max)}
              aria-pressed={selected === day}
              aria-label={`${day} · ${number(lookup.get(day) ?? 0)} 条活动`}
              title={`${day} · ${number(lookup.get(day) ?? 0)} 条活动`}
              onClick={() => setSelected(day)}
            />
          ))}
        </div>
      </div>
      <div className="dash-chart-axis">
        <span>{days[0]}</span>
        <span>{days.at(-1)}</span>
      </div>
      <div className="dash-calendar-legend">
        <span>少</span>
        {[0, 1, 2, 3, 4, 5].map((l) => (
          <i key={l} data-level={l} />
        ))}
        <span>多</span>
      </div>
      <p className="dash-chart-readout" aria-live="polite">
        {selected ? `${selected} · ${number(lookup.get(selected) ?? 0)} 条活动` : "点击日期查看活动数 · UTC"}
      </p>
      <p className="dash-note">按工具执行、推理与系统事件计数，反映任务执行活动。</p>
      <Link className="dash-text-link" href="/function/commands/">
        查看工具执行记录 <ArrowRight size={13} />
      </Link>
    </section>
  );
}

export default function DashboardPage() {
  const [revision, setRevision] = useState(0);
  const [days, setDays] = useState(7);
  const loadOverview = useCallback(() => api.dashboardOverview(days), [days]);
  const system = useDashboardSource(api.dashboardSystem, 2500, revision);
  const overview = useDashboardSource(loadOverview, 15000, revision);
  const tasks = useDashboardSource(loadTasks, 5000, revision);
  const traffic = useDashboardSource(loadTraffic, 15000, revision);
  const sys = system.data;
  const data = overview.data;
  const point = sys?.history.at(-1);
  const usage = usageTotals(data?.models ?? []);
  const total = usage.input + usage.output;
  const calls = (data?.models ?? []).reduce((n, row) => n + row.calls, 0);
  const findings = (data?.findings ?? []).reduce((n, row) => n + row.count, 0);
  const findingGroups = groupFindings(data?.findings ?? []);
  const assets = (data?.assets ?? []).reduce((n, row) => n + row.count, 0);
  const allTasks = tasks.data?.tasks ?? [];
  const running = allTasks.filter((t) => t.status === "running").length;
  const recentTasks = [...allTasks]
    .sort((a, b) => (b.last_activity_unix ?? b.created_unix ?? 0) - (a.last_activity_unix ?? a.created_unix ?? 0))
    .slice(0, 6);
  const trafficTotal = traffic.data?.total ?? traffic.data?.count ?? 0;
  const stale = system.error || overview.error || tasks.error || traffic.error;
  const connected = system.updated && overview.updated && tasks.updated && traffic.updated;
  const peak = (field: "cpu" | "memory") => Math.max(0, ...(sys?.history ?? []).map((p) => p[field] ?? 0)).toFixed(1);
  const severities = [
    "critical",
    "high",
    "medium",
    "low",
    ...new Set((data?.findings ?? []).map((f) => f.severity).filter((s) => !severityLabel[s])),
  ];
  const severityCounts = severities.map((severity) => ({
    severity,
    count: (data?.findings ?? []).filter((f) => f.severity === severity).reduce((n, f) => n + f.count, 0),
  }));
  return (
    <div
      className="chengjian-dashboard"
      data-testid="live-dashboard"
      data-content-padding="false"
      data-dashboard-layout="wide"
    >
      <header className="dash-topline">
        <div>
          <span className="dash-label">CHENGJIAN / OPERATIONS</span>
          <span className="dash-top-title">运行总览</span>
        </div>
        <div className="dash-toolbar">
          <span className={`dash-live ${stale ? "is-stale" : ""}`} role="status">
            <i />
            {stale ? "部分数据延迟" : connected ? "LIVE" : "连接中"}
          </span>
          <button
            type="button"
            onClick={() => setRevision((n) => n + 1)}
            disabled={system.loading || overview.loading || tasks.loading || traffic.loading}
            aria-label="刷新仪表板"
          >
            <RefreshCw size={13} />
            刷新
          </button>
        </div>
      </header>
      <div className="dash-wrap">
        <section className="dash-host">
          <div>
            <div className="dash-label">HOST / 主机</div>
            <h1>{sys?.hostname || "CHENGJIAN"}</h1>
            <p>
              {sys
                ? `${sys.cpu_model || "CPU 型号不可用"} · ${sys.threads} 线程 · ${sys.platform}`
                : "正在获取服务器主机信息…"}
            </p>
          </div>
          <div className="dash-host-stats">
            <div>
              <span className="dash-label">LOAD 1 / 5 / 15</span>
              <strong title="操作系统平均运行队列负载；Windows 不提供该指标">
                {sys?.load
                  ? [sys.load.load1, sys.load.load5, sys.load.load15].map((n) => n.toFixed(2)).join(" / ")
                  : "不支持"}
              </strong>
            </div>
            <div>
              <span className="dash-label">UPTIME</span>
              <strong>
                {sys
                  ? `${Math.floor(sys.uptime / 86400)}d ${Math.floor((sys.uptime % 86400) / 3600)}h ${Math.floor((sys.uptime % 3600) / 60)}m`
                  : "—"}
              </strong>
            </div>
            <div>
              <span className="dash-label">MEMORY</span>
              <strong>{sys?.memory_total ? `${bytes(sys.memory_used)} / ${bytes(sys.memory_total)}` : "—"}</strong>
            </div>
          </div>
        </section>
        <section className="dash-telemetry" aria-label="主机实时遥测">
          <article className="dash-tel">
            <header>
              <span className="dash-label">
                <Cpu size={12} />
                CPU
              </span>
              <strong className="dash-digits">
                <Count value={point?.cpu} decimals={1} />
              </strong>
              <span>%</span>
            </header>
            <Spark history={sys?.history ?? []} field="cpu" />
            <footer>
              <span>{sys?.threads ?? "—"} LOGICAL CORES</span>
              <span>PEAK {peak("cpu")}%</span>
            </footer>
          </article>
          <article className="dash-tel">
            <header>
              <span className="dash-label">
                <Layers size={12} />
                MEM
              </span>
              <strong className="dash-digits">
                <Count value={point?.memory} decimals={1} />
              </strong>
              <span>%</span>
            </header>
            <Spark history={sys?.history ?? []} field="memory" />
            <footer>
              <span>{sys?.memory_total ? bytes(sys.memory_used) : "—"} USED</span>
              <span>PEAK {peak("memory")}%</span>
            </footer>
          </article>
          <article className="dash-tel">
            <header>
              <span className="dash-label">
                <Network size={12} />
                NET ↓
              </span>
              <strong className="dash-digits">
                <Count value={point?.rx == null ? null : point.rx / 1024} decimals={1} />
              </strong>
              <span>KB/s</span>
            </header>
            <Spark history={sys?.history ?? []} field="rx" max={0} />
            <footer>
              <span>
                <ArrowDown size={10} /> {point?.rx == null ? "采样中" : `${bytes(point.rx)}/s`}
              </span>
              <span>
                <ArrowUp size={10} /> {point?.tx == null ? "采样中" : `${bytes(point.tx)}/s`}
              </span>
            </footer>
          </article>
          <article className="dash-tel dash-disks">
            <header>
              <span className="dash-label">
                <HardDrive size={12} />
                DISK
              </span>
              <span title={sys?.disks?.length ? `采样于 ${new Date(sys.disks_at).toLocaleTimeString()}` : undefined}>
                {sys?.unavailable.includes("disk") ? "采样延迟" : "USED / TOTAL"}
              </span>
            </header>
            {sys?.disks?.length ? (
              sys.disks.map((disk) => (
                <div
                  className="dash-disk"
                  key={disk.mount}
                  title={`${disk.mount} · ${bytes(disk.used)} / ${bytes(disk.total)}`}
                >
                  <span>{disk.mount}</span>
                  <div className={`dash-track ${disk.percent >= 95 ? "critical" : disk.percent >= 85 ? "high" : ""}`}>
                    <i style={{ width: `${disk.percent}%` }} />
                  </div>
                  <span>{disk.percent.toFixed(1)}%</span>
                </div>
              ))
            ) : (
              <Empty>磁盘信息暂不可用</Empty>
            )}
          </article>
        </section>
        <div className="dash-cores-header">
          <span className="dash-label">CORE LOAD / 每核利用率</span>
          <SourceState error={system.error} updated={system.updated} />
        </div>
        <div className="dash-cores">
          {sys?.cores.length ? (
            sys.cores.map((value, i) => (
              <span key={`core-${i}`} data-level={level(value, 100)} title={`逻辑核 ${i} · ${value.toFixed(1)}%`}>
                <small>{String(i).padStart(2, "0")}</small>
                {Math.round(value)}
                <small>%</small>
              </span>
            ))
          ) : (
            <p className="dash-note">正在建立 CPU 采样基线…</p>
          )}
        </div>
        <nav className="dash-overview-strip" aria-label="业务概览">
          {[
            {
              href: "/function/findings/",
              label: "安全发现",
              icon: ShieldCheck,
              value: overview.updated ? findings : null,
              note: `${severityCounts.filter((s) => s.severity === "critical" || s.severity === "high").reduce((n, s) => n + s.count, 0)} 个高危及严重`,
            },
            {
              href: "/function/assets/",
              label: "已识别资产",
              icon: Network,
              value: overview.updated ? assets : null,
              note: `${data?.assets.length ?? 0} 种资产类型`,
            },
            {
              href: "/function/tasks/",
              label: "在线任务",
              icon: Activity,
              value: tasks.updated ? allTasks.length : null,
              note: `${running} 个任务运行中`,
            },
            {
              href: "/function/traffic/",
              label: "代理流量",
              icon: Layers,
              value: traffic.updated ? trafficTotal : null,
              note: traffic.data?.enabled ? "流量采集已启用" : "流量采集未启用",
            },
          ].map((item) => (
            <Link key={item.label} href={item.href}>
              <span className="dash-label">
                <item.icon size={13} />
                {item.label}
                <ArrowUpRight size={12} />
              </span>
              <strong className="dash-digits">
                <Count value={item.value} />
              </strong>
              <small>{item.value == null ? "正在读取" : item.note}</small>
            </Link>
          ))}
        </nav>
        <section className="dash-panel dash-usage">
          <SectionTitle label="TOKEN USAGE" title="模型用量">
            {overview.loading && data?.days !== days ? (
              <span className="dash-source" role="status">
                正在读取 {days} 天用量…
              </span>
            ) : (
              <SourceState error={overview.error} updated={overview.updated} />
            )}
            <fieldset className="dash-range" aria-label="用量时间范围">
              {[7, 14, 30].map((n) => (
                <button type="button" key={n} aria-pressed={days === n} onClick={() => setDays(n)}>
                  {n} 天
                </button>
              ))}
            </fieldset>
          </SectionTitle>
          {data ? (
            <>
              <div className="dash-usage-top">
                <div>
                  <strong className="dash-digits" data-testid="token-total">
                    <Count value={total} short />
                  </strong>
                  <span>总 Token</span>
                  <small>输入 + 输出 · {data.days} 个 UTC 日</small>
                </div>
                <dl>
                  {[
                    { label: "输入", value: compact(usage.input) },
                    { label: "输出", value: compact(usage.output) },
                    { label: "缓存命中率", value: usage.input ? `${pct(usage.cache, usage.input).toFixed(1)}%` : "—" },
                    { label: "调用次数", value: number(calls) },
                  ].map((item) => (
                    <div key={item.label}>
                      <dt>{item.label}</dt>
                      <dd>{item.value}</dd>
                    </div>
                  ))}
                </dl>
              </div>
              <Composition usage={usage} />
              <UsageCharts key={data.days} data={data} />
              <div className="dash-model-header">
                <span className="dash-label">BY MODEL</span>
                <span>缓存命中包含于输入 · 所有调用均计入</span>
              </div>
              <div className="dash-table-scroll">
                <table className="dash-table dash-models">
                  <thead>
                    <tr>
                      <th>MODEL</th>
                      <th>CALLS</th>
                      <th>INPUT</th>
                      <th>OUTPUT</th>
                      <th>CACHE HIT</th>
                      <th>AVG LATENCY</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.models.map((model) => (
                      <tr key={model.model}>
                        <td>{model.model}</td>
                        <td>{number(model.calls)}</td>
                        <td title={number(model.input)}>{compact(model.input)}</td>
                        <td title={number(model.output)}>{compact(model.output)}</td>
                        <td>
                          <span className="dash-model-hit">
                            <i style={{ width: `${pct(model.cache, model.input)}%` }} />
                          </span>
                          {model.input ? `${pct(model.cache, model.input).toFixed(1)}%` : "—"}
                        </td>
                        <td>{(model.latency / 1000).toFixed(1)}s</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {!data.models.length && <Empty>所选时间范围暂无模型调用</Empty>}
              </div>
            </>
          ) : (
            <Empty>{overview.error ? "用量数据读取失败，将自动重试。" : "正在读取计量账本…"}</Empty>
          )}
        </section>
        <div className="dash-risk-row">
          <section className="dash-panel dash-findings">
            <SectionTitle label="FINDINGS" title="风险分布">
              <Link className="dash-text-link" href="/function/findings/">
                全部发现 <ArrowUpRight size={12} />
              </Link>
            </SectionTitle>
            {data ? (
              <>
                <div className="dash-severity-track">
                  {severityCounts.map((s) => (
                    <i
                      key={s.severity}
                      className={s.severity}
                      title={`${severityLabel[s.severity] ?? "未分级"} ${s.count}`}
                      style={{ width: `${pct(s.count, findings)}%` }}
                    />
                  ))}
                </div>
                <div className="dash-severity-legend">
                  {severityCounts.map((s) => (
                    <span key={s.severity} className={s.severity}>
                      <i />
                      {severityLabel[s.severity] ?? "未分级"} <b>{s.count}</b>
                    </span>
                  ))}
                </div>
                <div className="dash-table-scroll" tabIndex={0} role="region" aria-label="风险分布表，可滚动查看">
                  <table className="dash-table dash-findings-table">
                    <thead>
                      <tr>
                        <th>等级</th>
                        <th>漏洞类型</th>
                        <th>数量</th>
                        <th>占比</th>
                        <th>关联报告</th>
                      </tr>
                    </thead>
                    <tbody>
                      {findingGroups.map((row) => (
                        <tr key={`${row.severity}/${row.category}`}>
                          <td className={row.severity}>{severityLabel[row.severity] ?? "未分级"}</td>
                          <td>{row.category}</td>
                          <td>{row.count}</td>
                          <td>{pct(row.count, findings).toFixed(0)}%</td>
                          <td>
                            <details className="dash-report-links">
                              <summary>{row.count > 10 ? `最近 10 份 / 共 ${row.count} 份` : `查看 ${row.count} 份`}</summary>
                              <ul tabIndex={0} aria-label={`${severityLabel[row.severity] ?? "未分级"} · ${row.category}关联报告，可滚动查看`}>
                                {[...row.reports].sort((a, b) => b.id - a.id).slice(0, 10).map((report) => (
                                  <li key={report.id}>
                                    <Link href={`/function/findings/detail/?id=${report.id}`}>
                                      #{report.id} · {report.label}
                                      <ArrowUpRight size={11} />
                                    </Link>
                                  </li>
                                ))}
                              </ul>
                            </details>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                  {!findings && <Empty>暂无安全发现</Empty>}
                </div>
              </>
            ) : (
              <Empty>正在读取风险分布…</Empty>
            )}
          </section>
          {data ? (
            <ActivityCalendar data={data} />
          ) : (
            <section className="dash-panel">
              <SectionTitle label="ACTIVITY" title="执行活动" />
              <Empty>正在读取活动记录…</Empty>
            </section>
          )}
        </div>
        <div className="dash-bottom-row">
          <section className="dash-panel">
            <SectionTitle label="ASSETS" title="资产分布">
              <Link className="dash-text-link" href="/function/assets/">
                资产库 <ArrowUpRight size={12} />
              </Link>
            </SectionTitle>
            <ul className="dash-assets">
              {(data?.assets ?? []).map((row) => (
                <li key={row.type}>
                  <span>{assetLabel[row.type] ?? row.type}</span>
                  <div className="dash-track">
                    <i
                      style={{ width: `${pct(row.count, Math.max(1, ...(data?.assets ?? []).map((a) => a.count)))}%` }}
                    />
                  </div>
                  <b>{number(row.count)}</b>
                </li>
              ))}
            </ul>
            {data && !assets && <Empty>暂无已识别资产</Empty>}
          </section>
          <section className="dash-panel">
            <SectionTitle label="TASKS" title="最近任务">
              <SourceState error={tasks.error} updated={tasks.updated} />
            </SectionTitle>
            <ul className="dash-tasks">
              {recentTasks.map((task) => (
                <li key={task.id}>
                  <Link
                    href={`/function/tasks/detail/?id=${encodeURIComponent(task.id)}`}
                    title={task.name || task.description}
                  >
                    <b>{task.name || task.description || "未命名任务"}</b>
                    <small>
                      #{task.id} · {task.created_at.slice(0, 10)}
                    </small>
                  </Link>
                  <span
                    className={`dash-pill ${task.status === "running" || task.status === "done" ? "is-active" : ""}`}
                  >
                    {statusLabel[task.status] ?? task.status}
                  </span>
                </li>
              ))}
            </ul>
            {tasks.updated && !recentTasks.length && <Empty>暂无在线任务</Empty>}
            <Link className="dash-text-link" href="/function/tasks/">
              查看全部任务 <ArrowRight size={13} />
            </Link>
          </section>
          <section className="dash-panel">
            <SectionTitle label="PROCESSES" title="主机进程">
              <span className="dash-label">TOP 8 / RSS</span>
            </SectionTitle>
            <div className="dash-table-scroll">
              <table className="dash-table dash-processes">
                <thead>
                  <tr>
                    <th>NAME</th>
                    <th>PID</th>
                    <th>RSS / MB</th>
                  </tr>
                </thead>
                <tbody>
                  {(sys?.processes ?? []).map((proc) => (
                    <tr key={proc.pid}>
                      <td>{proc.name || "unknown"}</td>
                      <td>{proc.pid}</td>
                      <td>{(proc.rss / 1048576).toFixed(0)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            {!sys?.processes?.length && <Empty>进程信息暂不可用</Empty>}
            <p className="dash-note">
              {sys?.processes?.length
                ? `采样于 ${new Date(sys.processes_at).toLocaleTimeString()} · 每 30 秒更新`
                : "仅展示可读取的主机进程"}
            </p>
          </section>
        </div>
        <footer className="dash-footer">
          <span>
            主机 2.5s / 业务 15s 自动刷新 · {sys?.history.length ?? 0} 个真实采样点
            {system.updated ? ` · ${system.updated.toLocaleTimeString()}` : ""}
          </span>
          <span>业务范围：在线记录（不含冷归档） · 时间聚合 UTC</span>
        </footer>
      </div>
    </div>
  );
}
