"use client";

import { useEffect, useMemo, useRef, useState } from "react";

import { ExternalLink, LoaderCircle, RefreshCw } from "lucide-react";

import { CopyButton } from "@/components/copy-button";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { api } from "@/lib/api";
import type { AttackChainSource } from "@/lib/attack-chain";

export interface EvidenceTarget {
  findingId: string;
  document: string;
  line?: number;
  endLine?: number;
  label?: string;
  excerpt?: string;
  documentHash?: string;
}

export function evidenceHref(target: EvidenceTarget, contextTaskId?: string): string {
  const params = new URLSearchParams({ id: target.findingId, evidence: target.document });
  if (target.line && target.line > 0) params.set("line", String(target.line));
  if (target.endLine && target.endLine > 0) params.set("end_line", String(target.endLine));
  if (contextTaskId) params.set("context_task", contextTaskId);
  if (target.documentHash) params.set("evidence_hash", target.documentHash);
  return `/function/findings/detail/?${params}`;
}

export function sourceEvidenceTarget(source: AttackChainSource, chainFindingId: string): EvidenceTarget {
  const finding = /^finding:(\d+)\/(.+)$/.exec(source.locator);
  const document = finding?.[2] ?? (/^activity:\d+$/.test(source.locator) ? source.locator : "unknown");
  const line = source.line && source.line > 0 ? source.line : undefined;
  const excerpt = source.kind === "activity" ? undefined : source.text;
  return {
    findingId: finding?.[1] ?? chainFindingId,
    document,
    line,
    endLine: line && excerpt ? line + excerpt.replace(/\r\n?/g, "\n").split("\n").length - 1 : undefined,
    label: source.label,
    excerpt,
  };
}

interface EvidenceDocument {
  text: string;
  name: string;
  locator: string;
  stale: boolean;
}

interface LocatedRange {
  start?: number;
  end?: number;
  message: string;
  warning: boolean;
}

// A change detector for shared line locations, not a security signature.
function documentHash(text: string): string {
  let hash = 2166136261;
  for (let index = 0; index < text.length; index++) hash = Math.imul(hash ^ text.charCodeAt(index), 16777619);
  return (hash >>> 0).toString(16).padStart(8, "0");
}

function locate(text: string, target: EvidenceTarget): LocatedRange {
  const lines = text.split("\n");
  const excerpt = target.excerpt?.replace(/\r\n?/g, "\n").trim();
  const requested = target.line && Number.isInteger(target.line) ? target.line : undefined;
  if (!excerpt && target.documentHash && target.documentHash !== documentHash(text)) {
    return {
      message: "原文已在此定位链接创建后发生变化，旧行号可能失效。已展示当前完整证据，未高亮旧位置。",
      warning: true,
    };
  }
  if (excerpt) {
    const offset = requested ? lines.slice(0, requested - 1).join("\n").length + (requested > 1 ? 1 : 0) : 0;
    const expected = requested ? text.indexOf(excerpt, offset) : -1;
    const matchesRequestedLine =
      expected >= offset && expected < offset + (lines[(requested ?? 1) - 1]?.length ?? 0) + 1;
    const position = matchesRequestedLine ? expected : text.indexOf(excerpt);
    if (position < 0) {
      return {
        message: "原文与链条摘录不再完全一致，无法准确定位。已展示当前完整证据，未标记可能错误的行。",
        warning: true,
      };
    }
    const second = text.indexOf(excerpt, position + 1);
    if (!matchesRequestedLine && second >= 0) {
      return { message: "摘录在当前原文中出现多次，无法唯一定位。已展示完整证据，未标记可能错误的行。", warning: true };
    }
    const start = text.slice(0, position).split("\n").length;
    const end = start + excerpt.split("\n").length - 1;
    return {
      start,
      end,
      message:
        requested && start !== requested
          ? `原文位置已变化；按摘录重新定位至第 ${start}–${end} 行。`
          : `已定位原始证据第 ${start}–${end} 行。`,
      warning: Boolean(requested && start !== requested),
    };
  }
  if (requested) {
    const end = target.endLine ?? requested;
    if (requested < 1 || requested > lines.length || !Number.isInteger(end) || end < requested || end > lines.length) {
      return { message: "指定行号已超出当前原文范围，无法准确定位。", warning: true };
    }
    return {
      start: requested,
      end,
      message: `当前原文第 ${requested}–${end} 行；分享链接依据当前行号定位。`,
      warning: false,
    };
  }
  return { message: "以下为数据库中保存的完整原文。", warning: false };
}

async function loadDocument(target: EvidenceTarget, contextTaskId?: string): Promise<EvidenceDocument> {
  if (!/^\d+$/.test(target.findingId)) throw new Error("无效的漏洞编号。");
  const finding = await api.getFinding(target.findingId, contextTaskId);
  if (target.document === "report" || target.document === "evidence" || target.document === "summary") {
    const names = { report: "漏洞报告原文", evidence: "原始证据 / PoC", summary: "漏洞摘要原文" };
    return {
      text: finding[target.document] ?? "",
      name: names[target.document],
      locator: `finding:${target.findingId}/${target.document}`,
      stale: target.document === "report" && Boolean(finding.report_stale),
    };
  }
  if (/^reference\/(finding|fact|intent)\d+$/.test(target.document)) {
    const chain = await api.findingAttackChain(target.findingId, contextTaskId);
    const locator = `finding:${target.findingId}/${target.document}`;
    const source = chain.sources.find((candidate) => candidate.locator === locator);
    if (!source) throw new Error("对应的探索记录已不存在或不再属于当前攻击链。");
    return { text: source.text, name: "关联探索记录原文", locator, stale: false };
  }
  const activity = /^activity:(\d+)$/.exec(target.document);
  if (activity) {
    const chain = await api.findingAttackChain(target.findingId, contextTaskId);
    if (!chain.sources.some((source) => source.locator === target.document)) {
      throw new Error("此执行记录未关联到当前漏洞，无法作为其证据打开。");
    }
    // Activity ids are scoped by their original task, even when a finding is inherited.
    const taskId = finding.source_task_id ?? finding.task_id;
    if (!taskId) throw new Error("当前漏洞缺少来源任务，无法读取对应执行记录。");
    const activityId = Number(activity[1]);
    if (!Number.isSafeInteger(activityId)) throw new Error("无效的执行记录编号。");
    const detail = await api.activityDetail(activityId, taskId);
    return {
      text: detail.detail,
      name: `执行记录 #${activityId} · 完整 detail`,
      locator: target.document,
      stale: false,
    };
  }
  throw new Error("暂不支持此证据来源，无法跳转到未经确认的原文。");
}

export function EvidenceDocumentDialog({
  target,
  contextTaskId,
  onClose,
}: {
  target: EvidenceTarget | null;
  contextTaskId?: string;
  onClose: () => void;
}) {
  const targetKey = JSON.stringify(target);
  const stableTarget = useMemo(() => JSON.parse(targetKey) as EvidenceTarget | null, [targetKey]);
  const [document, setDocument] = useState<EvidenceDocument | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [retry, setRetry] = useState(0);
  const scrollRef = useRef<HTMLElement>(null);

  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry deliberately repeats the same read-only request.
  useEffect(() => {
    let cancelled = false;
    setDocument(null);
    setError("");
    if (!stableTarget) {
      setLoading(false);
      return;
    }
    setLoading(true);
    void loadDocument(stableTarget, contextTaskId)
      .then((loaded) => {
        if (!cancelled) setDocument({ ...loaded, text: loaded.text.replace(/\r\n?/g, "\n") });
      })
      .catch((reason: unknown) => {
        if (!cancelled) setError(reason instanceof Error ? reason.message : "读取证据失败。");
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [stableTarget, contextTaskId, retry]);

  const location = useMemo(
    () => (document && stableTarget ? locate(document.text, stableTarget) : null),
    [document, stableTarget],
  );
  const lines = useMemo(() => document?.text.split("\n") ?? [], [document]);
  useEffect(() => {
    if (!document || !location) return;
    const frame = requestAnimationFrame(() => {
      const viewport = scrollRef.current;
      const row = viewport?.querySelector<HTMLElement>(`[data-evidence-line="${location.start ?? 1}"]`);
      if (viewport && row) viewport.scrollTop = Math.max(0, row.offsetTop - 48);
    });
    return () => cancelAnimationFrame(frame);
  }, [document, location]);

  const resolvedTarget =
    stableTarget && document
      ? {
          ...stableTarget,
          line: location?.start,
          endLine: location?.end,
          documentHash: documentHash(document.text),
        }
      : stableTarget;
  const href = resolvedTarget ? evidenceHref(resolvedTarget, contextTaskId) : "";
  const shareLink = typeof window === "undefined" ? href : `${window.location.origin}${href}`;

  return (
    <Dialog
      open={Boolean(target)}
      onOpenChange={(open) => {
        if (!open) onClose();
      }}
    >
      <DialogContent className="gap-3 overflow-hidden sm:max-w-5xl">
        <DialogHeader className="pr-8">
          <DialogTitle>{stableTarget?.label ?? document?.name ?? "原始证据"}</DialogTitle>
          <DialogDescription className="break-all font-mono text-xs">
            {document?.locator ?? (stableTarget ? `finding:${stableTarget.findingId}/${stableTarget.document}` : "")}
          </DialogDescription>
        </DialogHeader>
        <div className="flex flex-wrap items-center gap-2 border-y py-2">
          <CopyButton text={document?.text} label="复制原文" size="sm" />
          <CopyButton text={document ? shareLink : null} label="复制定位链接" size="sm" />
          {document ? (
            <Button variant="ghost" size="sm" asChild>
              <a href={href} target="_blank" rel="noopener noreferrer">
                <ExternalLink />
                独立打开
              </a>
            </Button>
          ) : (
            <Button variant="ghost" size="sm" disabled>
              <ExternalLink />
              独立打开
            </Button>
          )}
          {document && (
            <span className="ml-auto text-muted-foreground text-xs">
              {document.name} · {lines.length} 行
            </span>
          )}
        </div>
        {loading && (
          <div role="status" className="flex min-h-48 items-center justify-center gap-2 text-muted-foreground">
            <LoaderCircle className="size-4 animate-spin" />
            正在读取完整证据
          </div>
        )}
        {error && (
          <div role="alert" className="space-y-3 py-8">
            <p>{error}</p>
            <Button variant="outline" onClick={() => setRetry((value) => value + 1)}>
              <RefreshCw />
              重试
            </Button>
          </div>
        )}
        {document && (
          <>
            <div role="status" className="space-y-1 text-muted-foreground text-xs">
              <p className={location?.warning ? "text-foreground" : undefined}>{location?.message}</p>
              {document.stale && <p>报告依据的证据版本已变化；以下展示当前已保存报告，尚未重新生成。</p>}
            </div>
            <section
              ref={scrollRef}
              // biome-ignore lint/a11y/noNoninteractiveTabindex: Keyboard users must be able to focus and scroll the evidence text.
              tabIndex={0}
              aria-label="可滚动的原始证据，左侧为行号"
              className="relative max-h-[58dvh] min-h-36 overflow-auto border-y bg-muted/10 font-mono text-xs leading-6 outline-offset-2"
            >
              {document.text ? (
                lines.map((line, index) => {
                  const number = index + 1;
                  const selected = Boolean(
                    location?.start && number >= location.start && number <= (location.end ?? location.start),
                  );
                  return (
                    <div
                      key={number}
                      data-evidence-line={number}
                      className={`flex min-w-0 border-l-2 ${selected ? "border-foreground bg-foreground/5" : "border-transparent"}`}
                    >
                      <span
                        aria-hidden="true"
                        className="sticky left-0 w-12 shrink-0 select-none bg-background px-2 text-right text-muted-foreground tabular-nums"
                      >
                        {number}
                      </span>
                      <span className="min-w-0 flex-1 whitespace-pre-wrap break-all px-3">{line || "\u00a0"}</span>
                    </div>
                  );
                })
              ) : (
                <p className="p-4 text-muted-foreground">数据库中尚未保存此文档的内容。</p>
              )}
            </section>
          </>
        )}
      </DialogContent>
    </Dialog>
  );
}
