export interface DashboardPoint {
  at: string;
  cpu: number | null;
  memory: number | null;
  rx: number | null;
  tx: number | null;
}

export interface DashboardSystem {
  sampled_at: string;
  hostname: string;
  platform: string;
  cpu_model: string;
  threads: number;
  uptime: number;
  load: { load1: number; load5: number; load15: number } | null;
  memory_total: number;
  memory_used: number;
  cores: number[];
  disks: { mount: string; total: number; used: number; percent: number }[];
  disks_at: string;
  processes: { pid: number; name: string; rss: number }[] | null;
  processes_at: string;
  history: DashboardPoint[];
  unavailable: string[];
}

export interface DashboardUsage {
  input: number;
  output: number;
  cache: number;
}

export interface DashboardOverview {
  sampled_at: string;
  timezone: string;
  days: number;
  findings: { severity: string; vulnclass: string; count: number; example_id: number; ids: number[] }[];
  assets: { type: string; count: number }[];
  models: (DashboardUsage & { model: string; calls: number; cache_write: number; latency: number })[];
  hourly: (DashboardUsage & { day: string; hour: number; calls: number })[];
  activity: { day: string; count: number }[];
}

export function utcDays(at: string, count: number) {
  const end = new Date(new Date(at).toISOString().slice(0, 10));
  return Array.from({ length: count }, (_, index) => {
    const day = new Date(end);
    day.setUTCDate(day.getUTCDate() - count + index + 1);
    return day.toISOString().slice(0, 10);
  });
}

export function usageTotals(rows: DashboardUsage[]): DashboardUsage {
  return rows.reduce(
    (total, row) => ({
      input: total.input + row.input,
      output: total.output + row.output,
      cache: total.cache + row.cache,
    }),
    { input: 0, output: 0, cache: 0 },
  );
}

export function level(value: number, max: number) {
  if (!value || !max) return 0;
  const ratio = value / max;
  return [0.12, 0.3, 0.55, 0.8, Infinity].findIndex((limit) => ratio < limit) + 1;
}

export function findingCategory(value: string) {
  const categories: [RegExp, string][] = [
    [/认证绕过|authentication|auth.?bypass/i, "认证绕过"],
    [/未授权|broken.access|unauthorized|越权/i, "访问控制"],
    [/配置|config/i, "配置安全"],
    [/信息泄露|泄露|disclosure|leak/i, "信息泄露"],
    [/上传|upload/i, "文件上传"],
    [/注入|injection|jdbc|sql/i, "注入风险"],
    [/redis|cache/i, "缓存安全"],
    [/文件|file/i, "文件操作"],
  ];
  return categories.find(([pattern]) => pattern.test(value))?.[1] ?? "其他类型";
}

export function groupFindings(rows: DashboardOverview["findings"]) {
  const groups = new Map<
    string,
    { severity: string; category: string; count: number; reports: { id: number; label: string }[] }
  >();
  for (const row of rows) {
    const category = findingCategory(row.vulnclass);
    const key = `${row.severity}/${category}`;
    const group = groups.get(key) ?? { severity: row.severity, category, count: 0, reports: [] };
    group.count += row.count;
    group.reports.push(...row.ids.map((id) => ({ id, label: row.vulnclass || "未分类" })));
    groups.set(key, group);
  }
  return [...groups.values()];
}
