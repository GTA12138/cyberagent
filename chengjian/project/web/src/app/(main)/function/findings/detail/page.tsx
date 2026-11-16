"use client";

import * as React from "react";

import Link from "next/link";
import { useSearchParams } from "next/navigation";

import { ArrowLeftIcon, ArrowUpRightIcon, NetworkIcon, ShieldAlertIcon } from "lucide-react";
import { toast } from "sonner";

import { AttackChainView } from "@/components/attack-chain/attack-chain";
import { EvidenceDocumentDialog, type EvidenceTarget } from "@/components/attack-chain/evidence-document";
import { FindingRetestPanel } from "@/components/finding-retest-panel";
import { FindingTrafficPanel } from "@/components/finding-traffic-panel";
import { StatusBadge } from "@/components/status-badge";
import { Alert, AlertDescription } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Select, SelectContent, SelectGroup, SelectItem, SelectTrigger } from "@/components/ui/select";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { api } from "@/lib/api";
import { statusMeta } from "@/lib/status";
import type { Finding, FindingStatus, Severity } from "@/lib/types";

import { FindingLineageView } from "./lineage";
import styles from "./report-page.module.css";

const SEVERITIES: Severity[] = ["critical", "high", "medium", "low"];
const FINDING_STATUSES: FindingStatus[] = [
  "pending",
  "in_progress",
  "confirmed",
  "resolved",
  "fixed",
  "false_positive",
  "ignored",
  "duplicate",
  "risk_accepted",
];

function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN");
}

function FindingDetailInner() {
  const searchParams = useSearchParams();
  const id = searchParams.get("id") ?? "";
  const contextTaskId = searchParams.get("context_task") ?? "";
  const [finding, setFinding] = React.useState<Finding | null>(null);
  const [loaded, setLoaded] = React.useState(false);
  const [lineageOpen, setLineageOpen] = React.useState(false);
  const [trafficOpen, setTrafficOpen] = React.useState(false);
  const requestSeq = React.useRef(0);
  const evidenceTarget: EvidenceTarget | null = searchParams.get("evidence")
    ? {
        findingId: id,
        document: searchParams.get("evidence") || "report",
        line: searchParams.has("line") ? Number(searchParams.get("line")) : undefined,
        endLine: searchParams.has("end_line") ? Number(searchParams.get("end_line")) : undefined,
        documentHash: searchParams.get("evidence_hash") || undefined,
      }
    : null;

  const load = React.useCallback(() => {
    const seq = ++requestSeq.current;
    if (!id) {
      setLoaded(true);
      return;
    }
    api
      .getFinding(id, contextTaskId || undefined)
      .then((f) => {
        if (seq === requestSeq.current) setFinding(f);
      })
      .catch(() => {
        if (seq === requestSeq.current) setFinding(null);
      })
      .finally(() => {
        if (seq === requestSeq.current) setLoaded(true);
      });
  }, [contextTaskId, id]);
  React.useEffect(() => {
    setFinding(null);
    setLoaded(false);
    setLineageOpen(false);
    setTrafficOpen(false);
    load();
    return () => {
      requestSeq.current++;
    };
  }, [load]);

  const changeSeverity = React.useCallback(
    async (next: Severity) => {
      if (!finding || finding.inherited || next === finding.severity) return;
      const prev = finding.severity;
      setFinding({ ...finding, severity: next });
      try {
        const updated = await api.setFindingSeverity(id, next);
        setFinding(updated);
        toast.success(`严重等级已改为「${statusMeta("severity", next).label}」`);
      } catch (e) {
        setFinding((cur) => (cur ? { ...cur, severity: prev } : cur));
        toast.error(`更新失败：${(e as Error).message}`);
      }
    },
    [finding, id],
  );

  const changeStatus = React.useCallback(
    async (next: FindingStatus) => {
      if (!finding || finding.inherited || next === finding.status) return;
      const prev = finding.status;
      setFinding({ ...finding, status: next });
      try {
        const updated = await api.setFindingStatus(id, next);
        setFinding(updated);
        toast.success(`处理状态已改为「${statusMeta("finding", next).label}」`);
      } catch (e) {
        setFinding((cur) => (cur ? { ...cur, status: prev } : cur));
        toast.error(`更新失败：${(e as Error).message}`);
      }
    },
    [finding, id],
  );

  if (!finding) {
    return (
      <div className="flex flex-1 flex-col items-center justify-center gap-3 p-10 text-center">
        <p className="text-muted-foreground">{loaded ? `未找到发现 ${id}` : "加载中…"}</p>
        {loaded && (
          <Button asChild variant="outline">
            <Link href="/function/findings">
              <ArrowLeftIcon /> 返回发现列表
            </Link>
          </Button>
        )}
      </div>
    );
  }

  const title = finding.name || finding.vulnclass || "未分类";

  return (
    <div
      className={styles.page}
      data-content-padding="false"
      data-severity={finding.severity}
      data-status={finding.status}
    >
      <header className={styles.header}>
        <div className="flex flex-wrap items-center gap-2">
          <SidebarTrigger className="-ml-1" />
          <Button asChild variant="ghost" size="icon" className="size-7">
            <Link href="/function/findings" aria-label="返回发现列表">
              <ArrowLeftIcon />
            </Link>
          </Button>
          <ShieldAlertIcon className="size-4 shrink-0 text-muted-foreground" />
          <code className="font-mono text-muted-foreground text-xs">FINDING #{finding.id}</code>
          <div className="flex flex-wrap items-center gap-2 sm:ml-auto">
            {finding.inherited ? (
              <>
                <StatusBadge domain="severity" value={finding.severity} dot className={styles.severity} />
                <StatusBadge domain="finding" value={finding.status} dot className={styles.status} />
                <Badge variant="outline">来源任务 #{finding.source_task_id || finding.task_id} · 只读</Badge>
              </>
            ) : (
              <>
                <Select value={finding.severity} onValueChange={(v) => changeSeverity(v as Severity)}>
                  <SelectTrigger size="sm" className="h-7 w-auto" aria-label="严重等级">
                    <StatusBadge domain="severity" value={finding.severity} dot className={styles.severity} />
                  </SelectTrigger>
                  <SelectContent position="popper" align="end">
                    <SelectGroup>
                      {SEVERITIES.map((severity) => (
                        <SelectItem key={severity} value={severity}>
                          {statusMeta("severity", severity).label}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
                <Select value={finding.status} onValueChange={(v) => changeStatus(v as FindingStatus)}>
                  <SelectTrigger size="sm" className="h-7 w-auto" aria-label="处理状态">
                    <StatusBadge domain="finding" value={finding.status} dot className={styles.status} />
                  </SelectTrigger>
                  <SelectContent position="popper" align="end">
                    <SelectGroup>
                      {FINDING_STATUSES.map((status) => (
                        <SelectItem key={status} value={status}>
                          {statusMeta("finding", status).label}
                        </SelectItem>
                      ))}
                    </SelectGroup>
                  </SelectContent>
                </Select>
              </>
            )}
            <Button variant="ghost" size="sm" onClick={() => setLineageOpen(true)}>
              <NetworkIcon /> 探索图谱
            </Button>
          </div>
        </div>
        <h1 className={styles.title}>{title}</h1>
        <div className="flex flex-wrap items-center gap-x-5 gap-y-1 text-muted-foreground text-xs">
          <span>{finding.vulnclass || "未分类"}</span>
          {finding.task_id ? (
            <Link
              href={`/function/tasks/detail?id=${finding.task_id}`}
              title={finding.task_description}
              className="inline-flex min-w-0 items-center gap-1 hover:text-foreground hover:underline"
            >
              <span className="break-words">任务 #{finding.task_id} · 查看所属任务</span>
              <ArrowUpRightIcon className="size-3 shrink-0" />
            </Link>
          ) : (
            <span>所属任务已删除</span>
          )}
          <span className="tabular-nums">发现于 {fmtTime(finding.ts)}</span>
        </div>
      </header>
      <main className={styles.main}>
        <AttackChainView
          key={`${id}:${contextTaskId}`}
          findingId={id}
          contextTaskId={contextTaskId || undefined}
          finding={finding}
          onRefresh={load}
          onManageTraffic={() => setTrafficOpen(true)}
        />
        {finding.inherited ? (
          <Alert>
            <AlertDescription>此漏洞继承自来源任务，当前上下文仅供查看。请前往来源任务管理漏洞复测。</AlertDescription>
          </Alert>
        ) : (
          <FindingRetestPanel key={id} findingId={id} findingName={title} onCompleted={load} />
        )}
      </main>
      <Dialog open={lineageOpen} onOpenChange={setLineageOpen}>
        <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-[min(90rem,calc(100vw-2rem))]">
          <DialogHeader>
            <DialogTitle>探索图谱 · 漏洞 #{finding.id}</DialogTitle>
            <DialogDescription>查看任务探索节点与当前漏洞的来源关系。</DialogDescription>
          </DialogHeader>
          {lineageOpen && <FindingLineageView findingId={finding.id} />}
        </DialogContent>
      </Dialog>
      <Dialog
        open={trafficOpen}
        onOpenChange={(open) => {
          setTrafficOpen(open);
          if (!open) load();
        }}
      >
        <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-[min(75rem,calc(100vw-2rem))]">
          <DialogHeader>
            <DialogTitle>流量证据管理 · 漏洞 #{finding.id}</DialogTitle>
            <DialogDescription>管理当前漏洞绑定的请求与响应快照。</DialogDescription>
          </DialogHeader>
          {trafficOpen && (
            <FindingTrafficPanel
              key={id}
              findingId={id}
              contextTask={contextTaskId || undefined}
              readOnly={finding.inherited}
              onChanged={load}
            />
          )}
        </DialogContent>
      </Dialog>
      <EvidenceDocumentDialog
        target={evidenceTarget}
        contextTaskId={contextTaskId || undefined}
        onClose={() => {
          const url = new URL(window.location.href);
          for (const key of ["evidence", "line", "end_line", "evidence_hash"]) url.searchParams.delete(key);
          window.history.replaceState(null, "", url);
        }}
      />
    </div>
  );
}
// useSearchParams must sit under a Suspense boundary for static export.
export default function FindingDetailPage() {
  return (
    <React.Suspense fallback={null}>
      <FindingDetailInner />
    </React.Suspense>
  );
}
