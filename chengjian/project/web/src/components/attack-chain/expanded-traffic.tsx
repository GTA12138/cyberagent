"use client";

import { useCallback, useEffect, useRef, useState } from "react";

import { Download, LoaderCircle, RefreshCw } from "lucide-react";

import { CopyButton } from "@/components/copy-button";
import { Button } from "@/components/ui/button";
import { api } from "@/lib/api";
import type {
  EvidenceBodyPreview,
  FindingTrafficBinding,
  FindingTrafficDetail,
  TrafficEvidenceRole,
} from "@/lib/types";

import styles from "./expanded-traffic.module.css";

const ROLE_LABEL: Record<TrafficEvidenceRole, string> = {
  baseline: "正常对照",
  proof: "漏洞证明",
  verification: "补充验证",
  supporting: "辅助证据",
};

interface DetailState {
  detail?: FindingTrafficDetail;
  error?: string;
  loading: boolean;
}

function message(error: unknown): string {
  return error instanceof Error ? error.message : "读取证据失败，请重试。";
}

function CompleteHttpSide({
  findingId,
  contextTaskId,
  binding,
  side,
  initial,
}: {
  findingId: string;
  contextTaskId?: string;
  binding: FindingTrafficBinding;
  side: "request" | "response";
  initial: EvidenceBodyPreview;
}) {
  const [body, setBody] = useState(initial);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const [downloadError, setDownloadError] = useState("");
  const [downloading, setDownloading] = useState(false);
  const [retry, setRetry] = useState(0);
  const completed = useRef(initial);
  const label = side === "request" ? "请求" : "响应";
  const head = side === "request" ? binding.snapshot.req_head : binding.snapshot.resp_head;
  const fullText = `${head ?? ""}\r\n\r\n${body.binary ? "" : body.content}`;

  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry resumes the failed pagination from the last retained page.
  useEffect(() => {
    let active = true;
    setError("");
    async function complete() {
      let current = completed.current;
      if (current.binary || !current.truncated) return;
      setLoading(true);
      try {
        while (active && current.truncated && !current.binary) {
          const next = await api.findingTrafficBody(findingId, binding.id, side, current.next_offset, contextTaskId);
          if (!active) return;
          if (next.offset !== current.next_offset || (next.truncated && next.next_offset <= current.next_offset)) {
            throw new Error("正文分页位置不连续，已保留成功读取的内容，请重试或下载原始正文。");
          }
          if (next.total !== current.total) throw new Error("证据正文长度发生变化，无法确认分页完整性。");
          if (next.next_offset > next.total || (!next.truncated && next.next_offset !== next.total)) {
            throw new Error("正文结束位置与保存的长度不符，无法确认内容完整，请下载原始正文核验。");
          }
          current = { ...next, offset: 0, content: current.content + (next.binary ? "" : next.content) };
          completed.current = current;
          setBody(current);
        }
      } catch (reason) {
        if (active) setError(message(reason));
      } finally {
        if (active) setLoading(false);
      }
    }
    void complete();
    return () => {
      active = false;
    };
  }, [findingId, binding.id, side, contextTaskId, retry]);

  async function download() {
    setDownloading(true);
    setDownloadError("");
    try {
      await api.downloadFindingTrafficBody(findingId, binding.id, side, contextTaskId);
    } catch (reason) {
      setDownloadError(message(reason));
    } finally {
      setDownloading(false);
    }
  }

  return (
    <section className={styles.httpSide} aria-label={`${label}原始报文`}>
      <div className={styles.sideHeading}>
        <span>{side === "request" ? "REQUEST" : "RESPONSE"}</span>
        <span>{body.total.toLocaleString()} B</span>
        <div className={styles.sideActions}>
          {!body.binary && (
            <CopyButton
              text={fullText}
              label={body.truncated ? "复制已加载部分" : "复制报文"}
              variant="ghost"
              size="sm"
            />
          )}
          <Button variant="ghost" size="sm" disabled={downloading} onClick={() => void download()}>
            <Download />
            {body.binary ? "下载二进制正文" : "下载原始正文"}
          </Button>
        </div>
      </div>
      <pre className={styles.code}>
        <code>
          <span className={styles.httpHead}>{head?.length ? head : "（未保存报文头）"}</span>
          {"\n\n"}
          {body.binary ? (
            <span className={styles.binary}>二进制正文 · {body.total.toLocaleString()} 字节，请下载原始文件查看。</span>
          ) : (
            body.content || "（空正文）"
          )}
        </code>
      </pre>
      {body.binary && <p className={styles.sideStatus}>已保留报文头；二进制正文不转换为文本。</p>}
      {loading && (
        <p role="status" className={styles.sideStatus}>
          <LoaderCircle className="size-4 animate-spin" />
          自动读取完整正文：{body.next_offset.toLocaleString()} / {body.total.toLocaleString()} B
        </p>
      )}
      {!loading && !error && !body.binary && !body.truncated && (
        <p className={styles.sideStatus}>完整正文已展开 · {body.total.toLocaleString()} B</p>
      )}
      {error && (
        <div role="alert" className={styles.sideError}>
          <p>{error} 当前内容尚未完整。</p>
          <Button variant="ghost" size="sm" onClick={() => setRetry((value) => value + 1)}>
            <RefreshCw />
            继续读取
          </Button>
        </div>
      )}
      {downloadError && (
        <p role="alert" className={styles.sideError}>
          {downloadError}
        </p>
      )}
    </section>
  );
}

interface ExpandedTrafficProps {
  findingId: string;
  contextTaskId?: string;
  onManage?: () => void;
}

export function ExpandedTraffic(props: ExpandedTrafficProps) {
  // A provenance change must discard all previous evidence immediately, before effects run.
  return <ExpandedTrafficContent key={`${props.findingId}:${props.contextTaskId ?? ""}`} {...props} />;
}

function ExpandedTrafficContent({ findingId, contextTaskId, onManage }: ExpandedTrafficProps) {
  const [bindings, setBindings] = useState<FindingTrafficBinding[] | null>(null);
  const [details, setDetails] = useState<Record<string, DetailState>>({});
  const [error, setError] = useState("");
  const [retry, setRetry] = useState(0);
  const generation = useRef(0);
  const loadDetail = useCallback(
    async (bindingId: string, expectedGeneration: number) => {
      setDetails((current) => ({ ...current, [bindingId]: { ...current[bindingId], loading: true, error: "" } }));
      try {
        const detail = await api.findingTrafficDetail(findingId, bindingId, contextTaskId);
        if (generation.current === expectedGeneration)
          setDetails((current) => ({ ...current, [bindingId]: { detail, loading: false } }));
      } catch (reason) {
        if (generation.current === expectedGeneration)
          setDetails((current) => ({
            ...current,
            [bindingId]: { ...current[bindingId], error: message(reason), loading: false },
          }));
      }
    },
    [findingId, contextTaskId],
  );

  // biome-ignore lint/correctness/useExhaustiveDependencies: Retry intentionally reloads the evidence binding list.
  useEffect(() => {
    const expectedGeneration = ++generation.current;
    setBindings(null);
    setDetails({});
    setError("");
    async function load() {
      try {
        const result = await api.findingTraffic(findingId, contextTaskId);
        if (generation.current !== expectedGeneration) return;
        const ordered = [...(result.bindings ?? [])].sort((a, b) => a.position - b.position);
        setBindings(ordered);
        for (const binding of ordered) {
          if (generation.current !== expectedGeneration) return;
          await loadDetail(binding.id, expectedGeneration);
        }
      } catch (reason) {
        if (generation.current === expectedGeneration) setError(message(reason));
      }
    }
    void load();
    return () => {
      generation.current++;
    };
  }, [findingId, contextTaskId, loadDetail, retry]);

  return (
    <section className={styles.traffic} aria-labelledby={`traffic-title-${findingId}`}>
      <header className={styles.heading}>
        <div>
          <span className={styles.eyebrow}>CAPTURED HTTP EVIDENCE</span>
          <h3 id={`traffic-title-${findingId}`}>绑定流量快照 {bindings ? `· ${bindings.length}` : ""}</h3>
        </div>
        <div className={styles.headingActions}>
          <Button variant="ghost" size="sm" onClick={() => setRetry((value) => value + 1)}>
            <RefreshCw />
            刷新快照
          </Button>
          {onManage && (
            <Button variant="outline" size="sm" onClick={onManage}>
              管理绑定证据
            </Button>
          )}
        </div>
      </header>
      <p className={styles.description}>
        以下内容来自已绑定的真实 HTTP 请求与响应快照。请求示例和文字记录在完整技术报告中展示，不视为已捕获流量。
      </p>
      {error && (
        <div role="alert" className={styles.error}>
          <p>{error}</p>
          <Button variant="outline" size="sm" onClick={() => setRetry((value) => value + 1)}>
            <RefreshCw />
            重试
          </Button>
        </div>
      )}
      {!bindings && !error && (
        <p role="status" className={styles.loading}>
          <LoaderCircle className="size-4 animate-spin" />
          正在读取绑定证据
        </p>
      )}
      {bindings?.length === 0 && (
        <p className={styles.empty}>
          此漏洞尚未绑定 HTTP
          流量快照。报告中即使包含请求或响应文本，也属于报告记录，当前没有对应的原始流量快照可供核验。
        </p>
      )}
      {bindings?.map((binding, index) => {
        const state = details[binding.id];
        const detail = state?.detail;
        const savedBinding = detail?.binding ?? binding;
        return (
          <article id={`traffic-binding-${binding.id}`} key={binding.id} className={styles.exchange}>
            <header className={styles.exchangeHeading}>
              <span className={styles.sequence}>{String(index + 1).padStart(2, "0")}</span>
              <span className={styles.method}>{savedBinding.snapshot.method}</span>
              <span className={styles.url}>{savedBinding.snapshot.url}</span>
              <span className={savedBinding.snapshot.status >= 400 ? styles.statusWarning : styles.status}>
                {savedBinding.snapshot.status || "无状态码"}
              </span>
            </header>
            <div className={styles.metadata}>
              <span>真实快照 #{savedBinding.snapshot_id}</span>
              <span>{ROLE_LABEL[savedBinding.role]}</span>
              <span>来源流量 #{savedBinding.snapshot.source_traffic_id}</span>
              <span>绑定 #{binding.id}</span>
            </div>
            {savedBinding.note && <p className={styles.note}>{savedBinding.note}</p>}
            {state?.error && (
              <div role="alert" className={styles.error}>
                <p>{state.error}</p>
                <Button variant="outline" size="sm" onClick={() => void loadDetail(binding.id, generation.current)}>
                  <RefreshCw />
                  重试此快照
                </Button>
              </div>
            )}
            {(!state || state.loading) && (
              <p role="status" className={styles.loading}>
                <LoaderCircle className="size-4 animate-spin" />
                正在读取请求与响应
              </p>
            )}
            {detail && (
              <div className={styles.exchangeBody}>
                {(["request", "response"] as const).map((side) => (
                  <CompleteHttpSide
                    key={`${findingId}-${contextTaskId ?? ""}-${binding.id}-${detail.binding.snapshot[side === "request" ? "req_hash" : "resp_hash"]}-${side}`}
                    findingId={findingId}
                    contextTaskId={contextTaskId}
                    binding={detail.binding}
                    side={side}
                    initial={detail[side]}
                  />
                ))}
              </div>
            )}
          </article>
        );
      })}
    </section>
  );
}
