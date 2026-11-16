"use client";

import * as React from "react";

import { FileText, LoaderCircle, RefreshCw } from "lucide-react";

import { CopyButton } from "@/components/copy-button";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type { AttackChain, AttackChainSource } from "@/lib/attack-chain";
import { mergeReport, normalizeReport, type ReportSource } from "@/lib/finding-report";
import type { Finding } from "@/lib/types";

import styles from "./attack-chain.module.css";
import { EvidenceDocumentDialog, type EvidenceTarget, sourceEvidenceTarget } from "./evidence-document";
import { ExpandedTraffic } from "./expanded-traffic";
import { TemplateReport } from "./template-report";

/** The task view and finding detail consume the same live report renderer. */
export function AttackChainView({
  findingId,
  contextTaskId,
  finding: providedFinding,
  onRefresh,
  onManageTraffic,
}: {
  findingId: string;
  contextTaskId?: string;
  finding?: Finding;
  onRefresh?: () => void;
  onManageTraffic?: () => void;
}) {
  const [finding, setFinding] = React.useState<Finding | null>(null);
  const [chain, setChain] = React.useState<AttackChain | undefined>();
  const [error, setError] = React.useState("");
  const [chainError, setChainError] = React.useState("");
  const [refresh, setRefresh] = React.useState(0);
  const [target, setTarget] = React.useState<EvidenceTarget | null>(null);
  const currentFinding = providedFinding ?? finding;
  const report = React.useMemo(
    () => (currentFinding ? mergeReport(normalizeReport(currentFinding)) : null),
    [currentFinding],
  );

  // biome-ignore lint/correctness/useExhaustiveDependencies: Refresh repeats the read-only requests.
  React.useEffect(() => {
    let alive = true;
    setError("");
    setChainError("");
    const findingRequest = providedFinding
      ? Promise.resolve(providedFinding)
      : api.getFinding(findingId, contextTaskId);
    void findingRequest
      .then((next) => {
        if (alive) setFinding(next);
      })
      .catch((reason: Error) => {
        if (alive) setError(reason.message);
      });
    void api
      .findingAttackChain(findingId, contextTaskId)
      .then((next) => {
        if (alive) setChain(next);
      })
      .catch((reason: Error) => {
        if (alive) {
          setChain(undefined);
          setChainError(reason.message);
        }
      });
    return () => {
      alive = false;
    };
  }, [findingId, contextTaskId, providedFinding, refresh]);

  function openSource(source: ReportSource) {
    setTarget({
      findingId: source.findingId,
      document: source.document,
      line: source.line,
      endLine: source.endLine,
      excerpt: source.text,
    });
  }
  function openReference(source: AttackChainSource) {
    setTarget(sourceEvidenceTarget(source, findingId));
  }
  if (error && !currentFinding)
    return (
      <div className={styles.loading} role="alert">
        无法加载漏洞报告：{error}
        <Button variant="outline" onClick={() => setRefresh((value) => value + 1)}>
          重试
        </Button>
      </div>
    );
  if (!currentFinding || !report)
    return (
      <div className={styles.loading}>
        <LoaderCircle className="size-5 animate-spin" />
        正在读取完整漏洞报告…
      </div>
    );

  return (
    <div className={styles.workspace} data-testid="attack-chain-workspace" data-report-layout="wide">
      <div className={styles.toolbar}>
        <div>
          <span className={styles.eyebrow}>调查报告</span>
          <span className={styles.reportMeta}>
            {report.metrics.codeBlocks} 个代码块<span>·</span>
            {report.scope.endpoints.length} 个接口<span>·</span>按步骤查看
          </span>
        </div>
        <div className={styles.actions}>
          <CopyButton text={currentFinding.report ? currentFinding.report : currentFinding.evidence} label="复制报告" />
          <Button
            variant="outline"
            size="sm"
            onClick={() => setTarget({ findingId, document: currentFinding.report ? "report" : "evidence" })}
          >
            <FileText className="size-3.5" /> 原始记录
          </Button>
          <Button
            variant="outline"
            size="sm"
            onClick={() => {
              setRefresh((value) => value + 1);
              onRefresh?.();
            }}
          >
            <RefreshCw className="size-3.5" />
            刷新
          </Button>
        </div>
      </div>
      {currentFinding.report_stale && (
        <p className={styles.notice}>流量证据已变更，报告尚未同步更新。请结合最新流量快照核对。</p>
      )}
      {error && (
        <p className={styles.notice} role="alert">
          报告刷新失败，当前保留上次读取内容：{error}
        </p>
      )}
      {chainError && (
        <p className={styles.notice} role="alert">
          关联来源暂时不可用：{chainError}。完整报告仍可阅读。
        </p>
      )}
      <TemplateReport
        report={report}
        finding={currentFinding}
        chain={chain}
        onSource={openSource}
        onReference={openReference}
        contextTaskId={contextTaskId}
        traffic={
          <section className={styles.trafficSection} aria-label="已保存的原始流量">
            <ExpandedTraffic
              key={`${findingId}-${contextTaskId ?? ""}-${refresh}-${currentFinding.evidence_version ?? 0}`}
              findingId={findingId}
              contextTaskId={contextTaskId}
              onManage={onManageTraffic}
            />
          </section>
        }
      />
      <EvidenceDocumentDialog target={target} contextTaskId={contextTaskId} onClose={() => setTarget(null)} />
    </div>
  );
}
