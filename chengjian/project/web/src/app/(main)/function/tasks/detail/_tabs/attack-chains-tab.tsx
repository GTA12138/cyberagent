"use client";

import * as React from "react";

import { ChevronLeft, ChevronRight, LoaderCircle, Search } from "lucide-react";

import { AttackChainView } from "@/components/attack-chain/attack-chain";
import { api } from "@/lib/api";
import type { Finding } from "@/lib/types";

export function AttackChainsTab({ taskId }: { taskId: string }) {
  const [findings, setFindings] = React.useState<Finding[]>([]);
  const [selected, setSelected] = React.useState("");
  const [query, setQuery] = React.useState("");
  const [page, setPage] = React.useState(1);
  const [total, setTotal] = React.useState(0);
  const [error, setError] = React.useState("");
  const [loading, setLoading] = React.useState(true);
  const [revision, setRevision] = React.useState(0);
  React.useEffect(() => {
    let alive = true;
    void revision;
    setLoading(true);
    const timer = setTimeout(
      () => {
        api
          .findingsPage({ task: taskId, page, pageSize: 100, query, sort: "time" })
          .then((result) => {
            if (!alive) return;
            setFindings(result.items ?? []);
            setTotal(result.total);
            setError("");
            setSelected((previous) =>
              result.items.some((f) => (f.finding_id ?? f.id) === previous)
                ? previous
                : (result.items[0]?.finding_id ?? result.items[0]?.id ?? ""),
            );
          })
          .catch((e: Error) => {
            if (alive) setError(e.message);
          })
          .finally(() => {
            if (alive) setLoading(false);
          });
      },
      query ? 250 : 0,
    );
    return () => {
      alive = false;
      clearTimeout(timer);
    };
  }, [taskId, page, query, revision]);
  return (
    <div>
      <div className="mb-5 flex flex-wrap items-center gap-3 rounded-md border bg-card p-3">
        <label className="flex min-w-48 flex-1 items-center gap-2 text-muted-foreground text-xs">
          <Search className="size-3.5" />
          <input
            aria-label="搜索任务漏洞"
            placeholder="搜索漏洞名称、类型或证据…"
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setPage(1);
            }}
            className="min-w-0 flex-1 bg-transparent p-1 text-foreground outline-none"
          />
        </label>
        <label className="flex min-w-0 flex-2 items-center gap-2 text-xs">
          <span className="shrink-0 text-muted-foreground">选择漏洞</span>
          <select
            aria-label="选择要还原的漏洞"
            value={selected}
            onChange={(e) => setSelected(e.target.value)}
            className="min-w-0 flex-1 rounded border bg-background p-2 text-xs"
          >
            {findings.map((finding) => (
              <option key={finding.finding_id ?? finding.id} value={finding.finding_id ?? finding.id}>
                #{finding.finding_id ?? finding.id} · {finding.name || finding.vulnclass}
              </option>
            ))}
          </select>
        </label>
        <span className="shrink-0 text-muted-foreground text-xs">共 {total} 个漏洞</span>
        {loading && <LoaderCircle className="size-3.5 animate-spin text-muted-foreground" />}
        {total > 100 && (
          <div className="flex items-center gap-2 text-xs">
            <button type="button" aria-label="上一页漏洞" disabled={page === 1} onClick={() => setPage((p) => p - 1)}>
              <ChevronLeft className="size-4" />
            </button>
            {page} / {Math.ceil(total / 100)}
            <button
              type="button"
              aria-label="下一页漏洞"
              disabled={page * 100 >= total}
              onClick={() => setPage((p) => p + 1)}
            >
              <ChevronRight className="size-4" />
            </button>
          </div>
        )}
      </div>
      {error ? (
        <div role="alert" className="rounded border p-5 text-sm">
          {error}
          <button type="button" className="ml-3 underline" onClick={() => setRevision((v) => v + 1)}>
            重试
          </button>
        </div>
      ) : selected ? (
        <AttackChainView key={selected} findingId={selected} contextTaskId={taskId} />
      ) : (
        !loading && <div className="p-12 text-center text-muted-foreground text-sm">没有匹配的漏洞记录。</div>
      )}
    </div>
  );
}
