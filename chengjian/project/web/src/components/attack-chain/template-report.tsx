"use client";

import { type ReactElement, type ReactNode, useEffect, useId, useRef, useState } from "react";

import Link from "next/link";

import {
  ArrowDown,
  ArrowUpRight,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Database,
  FileCode2,
  FileText,
  FlaskConical,
  GitBranch,
  Globe2,
  KeyRound,
  Link2,
  Network,
  Play,
  ScanSearch,
  ShieldAlert,
  ShieldOff,
  Square,
  Terminal,
  Upload,
  UserRound,
  Wrench,
} from "lucide-react";

import { CopyButton } from "@/components/copy-button";
import type { AttackChain, AttackChainSource } from "@/lib/attack-chain";
import type { MergedFindingReport, ReportBlock, ReportSource, ReportStep } from "@/lib/finding-report/types";
import type { Finding } from "@/lib/types";

import { comparisonRows, ReportComparison } from "./report-comparison";
import { ReportDocument } from "./report-document";
import { ReportPath, reportPath } from "./report-path";
import styles from "./template-report.module.css";

export interface TemplateReportProps {
  report: MergedFindingReport;
  chain?: AttackChain;
  finding?: Finding;
  contextTaskId?: string;
  onSource?: (source: ReportSource) => void;
  onRelated?: (findingId: string) => void;
  onReference?: (source: AttackChainSource) => void;
  traffic?: ReactElement;
}

function SourceButton({ source, onSource }: { source?: ReportSource; onSource?: TemplateReportProps["onSource"] }) {
  if (!source || !onSource) return null;
  return (
    <button type="button" className={styles.sourceButton} onClick={() => onSource(source)}>
      原文 L{source.line}–{source.endLine}
      <ArrowUpRight size={11} />
    </button>
  );
}

function MarkdownText({
  text,
  document = "report",
  blocks = [],
}: {
  text: string;
  document?: string;
  blocks?: ReportBlock[];
}) {
  if (!text.trim()) return null;
  return (
    <div className={styles.prose}>
      <ReportDocument text={text} document={document} />
      {blocks.some((block) => block.excerpted || !block.closed) && (
        <div className={styles.blockNotice}>
          {blocks.some((block) => block.excerpted) && <p>以上原文记录含摘录或省略标记；此处完整保留当前已存内容。</p>}
          {blocks.some((block) => !block.closed) && <p>原文代码围栏未闭合，已完整展示已存内容。</p>}
        </div>
      )}
    </div>
  );
}

function CodePanel({ block, onSource }: { block: ReportBlock; onSource?: TemplateReportProps["onSource"] }) {
  const labels: Record<string, string> = {
    "http-request": "HTTP 请求",
    "http-response": "HTTP 响应",
    "http-headers": "HTTP 头部",
    curl: "cURL 命令",
    shell: "Shell 命令",
    json: "JSON",
    output: "输出",
    code: "代码",
  };
  return (
    <div className={styles.codeBlock}>
      <div className={styles.blockHeading}>
        <span>
          {block.kind === "curl" || block.kind === "shell" ? <Terminal size={14} /> : <FileCode2 size={14} />}
          {labels[block.kind] ?? "原文"}
          {block.lang ? ` / ${block.lang}` : ""}
        </span>
        <SourceButton source={block.source} onSource={onSource} />
        <CopyButton text={block.body} label="复制" variant="ghost" size="sm" className={styles.copy} />
      </div>
      <pre
        className={styles.code}
        data-report-block={block.kind}
        data-origin-document={block.source.document}
        data-origin-line={block.source.line}
      >
        <code>{block.body}</code>
      </pre>
      {(block.excerpted || !block.closed || block.warnings.length > 0) && (
        <div className={styles.blockNotice}>
          {block.excerpted && <p>原文记录含摘录或省略标记；此处完整保留当前已存内容。</p>}
          {!block.closed && <p>原文代码围栏未闭合，已完整展示已存内容。</p>}
          {block.warnings.map((warning) => (
            <p key={warning}>{warning}</p>
          ))}
        </div>
      )}
    </div>
  );
}

function Pane({
  title,
  blocks,
  onSource,
}: {
  title: string;
  blocks: ReportBlock[];
  onSource?: TemplateReportProps["onSource"];
}) {
  if (!blocks.length) return null;
  return (
    <section className={styles.pane}>
      <header className={styles.paneHeading}>
        <h4>{title}</h4>
        <span>{blocks.length} 段 · 报告记录</span>
      </header>
      <div className={styles.paneBody}>
        {blocks.map((block, index) => (
          <CodePanel key={`${block.source.document}-${block.source.line}-${index}`} block={block} onSource={onSource} />
        ))}
      </div>
    </section>
  );
}

function balancedColumns(step: ReportStep): boolean {
  if (!step.request.length || !step.response.length) return false;
  const request = step.request.map((block) => block.body).join("\n");
  const response = step.response.map((block) => block.body).join("\n");
  const height = (text: string) =>
    text.split("\n").reduce((sum, line) => sum + Math.max(1, Math.ceil(line.length / 64)), 0);
  const left = height(request);
  const right = height(response);
  return (
    request.length <= 1500 &&
    response.length <= 1500 &&
    Math.max(left, right) <= 18 &&
    Math.max(left, right) <= Math.max(4, Math.min(left, right) * 2)
  );
}

function stepAction(step: ReportStep) {
  if (/对照|消融|控制组|无租户|有无|条件组合/.test(step.label))
    return { icon: FlaskConical, label: "条件对照", behavior: "comparison" };
  if (/上传|写入|修改|删除|save|upload/i.test(step.label))
    return { icon: Upload, label: "写入与变更", behavior: "risk" };
  if (/前端|JavaScript|\bJS\b|脚本|源码|硬编码/i.test(step.label))
    return { icon: FileCode2, label: "脚本与来源", behavior: "source" };
  if (/工号|员工|身份|用户|主体/.test(step.label))
    return { icon: UserRound, label: "主体与身份", behavior: "identity" };
  if (/数据|数据库|预警|配置|读取|响应|回显/.test(step.label))
    return { icon: Database, label: "数据与回显", behavior: "data" };
  if (/服务|接口|入口|横扫|目标|资产/.test(step.label))
    return { icon: Network, label: "服务与入口", behavior: "network" };

  if (/签名|密钥|凭据|secret|token/i.test(step.label))
    return { icon: KeyRound, label: "凭据与签名", behavior: "credential" };
  if (/认证|鉴权|绕过|登录/.test(step.label)) return { icon: ShieldOff, label: "身份与鉴权", behavior: "risk" };
  if (step.request.some((block) => block.kind === "http-request" || block.kind === "curl"))
    return { icon: Terminal, label: "请求与交互", behavior: "request" };
  if (step.other.some((block) => /javascript|js|html/i.test(block.lang)))
    return { icon: FileCode2, label: "脚本与来源", behavior: "source" };
  return { icon: ScanSearch, label: "调查与验证", behavior: "investigation" };
}

function chainTitle(step: ReportStep): string {
  // The navigator names the action; the unabridged record remains in the step below.
  if (step.label.length <= 48) return step.label;
  if (/计算签名|canonical|HMAC|X-Auth-Signature/i.test(step.label)) return "计算请求签名";
  if (/getSignKey|取密钥|获取.*密钥/i.test(step.label)) return "获取签名密钥";
  if (/未授权.*读取/.test(step.label)) {
    if (/Groovy|expression/i.test(step.label)) return "未授权读取表达式源码";
    if (/数据源|data-source/i.test(step.label)) return "未授权读取数据源配置";
    if (/预警记录|warning-record/i.test(step.label)) return "未授权读取预警记录";
    if (/预警规则|warning-rule/i.test(step.label)) return "未授权读取预警规则";
    return "未授权读取数据";
  }
  if (/未授权.*写入|addExpression|warning-rule\/save|warning-expression-config\/save/i.test(step.label))
    return "未授权写入配置";
  if (/前端|JavaScript|\bJS\b|脚本.*(?:定位|来源)/i.test(step.label)) return "分析前端脚本与来源";
  if (/对照|控制组/.test(step.label)) return "对照验证访问条件";
  return stepAction(step).label;
}

function Title({ eyebrow, title, detail }: { eyebrow: string; title: string; detail?: string }) {
  return (
    <div className={styles.sectionHead}>
      <h2>
        {eyebrow === "SCOPE" ? <Globe2 size={21} /> : eyebrow === "RELATED EVIDENCE" ? <GitBranch size={21} /> : null}
        {title}
      </h2>
      {detail && <p>{detail}</p>}
    </div>
  );
}

function directoryOffset() {
  return window.matchMedia("(max-width: 1150px)").matches ? 116 : 84;
}

function fitStepDirectory(directory: HTMLElement) {
  const top = Math.max(directoryOffset(), directory.getBoundingClientRect().top);
  directory.style.maxHeight = `${Math.max(0, window.innerHeight - top - 16)}px`;
}

function DisclosureSection({
  eyebrow,
  title,
  detail,
  collapsible = true,
  children,
}: {
  eyebrow: string;
  title: string;
  detail?: string;
  collapsible?: boolean;
  children: ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const panelId = useId();
  if (!collapsible)
    return (
      <section className={styles.section}>
        <Title eyebrow={eyebrow} title={title} detail={detail} />
        {children}
      </section>
    );
  return (
    <section className={styles.section} data-report-disclosure={eyebrow}>
      <h2 className={styles.disclosureHeading}>
        <button
          type="button"
          className={styles.disclosureToggle}
          aria-expanded={open}
          aria-controls={open ? panelId : undefined}
          onClick={() => setOpen((previous) => !previous)}
        >
          <span className={styles.disclosureLabel}>
            <span className={styles.disclosureTitle}>
              {eyebrow === "PROOF OF CONCEPT" ? <Terminal size={18} /> : <FileText size={18} />}
              {title}
            </span>
            {detail && <span className={styles.disclosureDetail}>{detail}</span>}
          </span>
          <span className={styles.disclosureAction}>
            {open ? "收起" : "展开"}
            <ChevronDown size={15} />
          </span>
        </button>
      </h2>
      {open && (
        <div id={panelId} data-report-disclosure-body>
          {children}
        </div>
      )}
    </section>
  );
}

export function TemplateReport({
  report,
  chain,
  finding,
  contextTaskId,
  onSource,
  onRelated,
  onReference,
  traffic,
}: TemplateReportProps) {
  const steps = report.mergedSteps;
  const hasScope =
    report.sections.some((section) => section.source.document === "report" && section.key === "scope") ||
    report.scope.endpoints.length > 0 ||
    !!finding?.assets?.length ||
    !!report.identity.asset.trim();
  const hasPath = steps.length > 0;
  const path = reportPath(report);
  const playbackLength = path.inputs.length + path.actions.length;
  const [selection, setSelection] = useState({ id: "", revision: 0, scroll: false });
  const activeIndex = Math.max(
    0,
    steps.findIndex((step) => step.id === selection.id),
  );
  const activeStep = steps[activeIndex];
  const readerRef = useRef<HTMLElement>(null);
  const stepRef = useRef<HTMLElement>(null);
  const directoryRef = useRef<HTMLElement>(null);
  const readerTitleId = useId();
  const hasSteps = steps.length > 0;
  const sectionPrefix = useId().replace(/:/g, "");
  const contentRef = useRef<HTMLDivElement>(null);
  const [activeSection, setActiveSection] = useState("overview");
  const [playing, setPlaying] = useState(false);
  const [playIndex, setPlayIndex] = useState(-1);
  const sectionId = (name: string) => sectionPrefix + "-" + name;
  const sectionNames = [
    { id: "overview", label: "调查概览", icon: ScanSearch },
    ...(hasScope ? [{ id: "scope", label: "受影响范围", icon: Globe2 }] : []),
    ...(hasPath ? [{ id: "path", label: "攻击路径", icon: GitBranch }] : []),
    { id: "evidence", label: "复现与证据", icon: Terminal },
    ...(traffic ? [{ id: "traffic", label: "原始流量", icon: Network }] : []),
    { id: "disposition", label: "PoC 与处置", icon: Wrench },
  ];
  useEffect(() => {
    const root = contentRef.current;
    if (!root) return;
    let frame = 0;
    const update = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => {
        const sections = [...root.querySelectorAll<HTMLElement>("[data-report-section]")];
        let current = sections[0]?.dataset.reportSection ?? "overview";
        for (const section of sections)
          if (section.getBoundingClientRect().top <= 150) current = section.dataset.reportSection ?? current;
        setActiveSection(current);
      });
    };
    update();
    window.addEventListener("scroll", update, { capture: true, passive: true });
    window.addEventListener("resize", update);
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("scroll", update, true);
      window.removeEventListener("resize", update);
    };
  }, []);
  useEffect(() => {
    if (!playing || !playbackLength) return;
    let index = 0;
    setPlayIndex(0);
    const timer = window.setInterval(() => {
      index += 1;
      if (index >= playbackLength) {
        setPlaying(false);
        setPlayIndex(-1);
      } else setPlayIndex(index);
    }, 1500);
    return () => window.clearInterval(timer);
  }, [playing, playbackLength]);
  function goToSection(name: string) {
    const target = document.getElementById(sectionId(name));
    if (!target) return;
    target.scrollIntoView({
      block: "start",
      behavior: window.matchMedia("(prefers-reduced-motion: reduce)").matches ? "instant" : "smooth",
    });
    setActiveSection(name);
  }

  useEffect(() => {
    if (!hasSteps) return;
    const directory = directoryRef.current;
    if (!directory) return;
    let frame = 0;
    const fit = () => {
      cancelAnimationFrame(frame);
      frame = requestAnimationFrame(() => fitStepDirectory(directory));
    };
    fit();
    window.addEventListener("resize", fit);
    window.addEventListener("scroll", fit, { capture: true, passive: true });
    return () => {
      cancelAnimationFrame(frame);
      window.removeEventListener("resize", fit);
      window.removeEventListener("scroll", fit, true);
    };
  }, [hasSteps]);
  useEffect(() => {
    if (!selection.revision) return;
    if (selection.scroll) readerRef.current?.scrollIntoView({ block: "start", behavior: "instant" });
    stepRef.current?.focus({ preventScroll: true });
    const directory = directoryRef.current;
    const current = directory?.querySelector('[aria-current="step"]');
    if (directory && current) {
      fitStepDirectory(directory);
      const parent = directory.getBoundingClientRect();
      const item = current.getBoundingClientRect();
      const visibleTop = Math.max(parent.top, directoryOffset());
      const visibleBottom = Math.min(parent.bottom, window.innerHeight - 16);
      if (item.top < visibleTop) directory.scrollTop += item.top - visibleTop;
      else if (item.bottom > visibleBottom) directory.scrollTop += item.bottom - visibleBottom;
    }
  }, [selection]);
  const pocBlocks = [...(report.poc.main ? [report.poc.main] : []), ...report.poc.extras];
  const pocLines = Math.max(
    report.poc.intro.split("\n").length,
    pocBlocks.reduce((lines, block) => lines + block.body.split("\n").length, 0),
  );
  const longPoc =
    pocLines > 24 ||
    Math.max(
      report.poc.intro.length,
      pocBlocks.reduce((length, block) => length + block.body.length, 0),
    ) > 1800;
  const references = chain?.nodes.filter((node) => !!node.reference) ?? [];
  const primarySections = report.sections.filter((section) => section.source.document === "report");
  const overview = primarySections.filter((section) => section.key === "overview");
  const impactSections = primarySections.filter((section) => section.key === "impact");
  const scopeSections = primarySections.filter((section) => section.key === "scope");
  const verdictText = overview.map((section) => section.text).join("\n\n") || report.raw.summary;
  const impactText = impactSections.map((section) => section.text).join("\n\n");
  const plain = (text: string) =>
    text
      .replace(/!\[[^\]]*\]\([^)]*\)/g, "")
      .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
      .replace(/^[#>*\s-]+/gm, "")
      .replace(/[*_`]/g, "")
      .replace(/\s+/g, " ")
      .trim();
  const preview = (text: string, limit: number) => {
    const value = plain(text);
    return value.length > limit ? value.slice(0, limit) + "…" : value;
  };
  const impactPoints = report.impacts.length
    ? report.impacts.map((item) => ({ lead: item.lead, body: item.body }))
    : impactText
        .split(/\n(?=\s*[-*]|\d+[.、])/)
        .filter((text) => plain(text))
        .map((text) => ({ lead: "报告记载", body: text }));
  const languages = [
    ...new Set(
      steps
        .flatMap((step) => [...step.request, ...step.response, ...step.output, ...step.other])
        .map((block) =>
          block.kind === "curl"
            ? "cURL"
            : block.kind.startsWith("http-")
              ? "HTTP"
              : block.lang || (block.kind === "json" ? "JSON" : "输出"),
        ),
    ),
  ].slice(0, 8);

  const structuredBlocks = [
    ...steps.flatMap((step) => [...step.request, ...step.response, ...step.output, ...step.other]),
    ...(report.poc.main ? [report.poc.main] : []),
    ...report.poc.extras,
  ];
  const sourceKey = (block: ReportBlock) => `${block.source.document}:${block.source.start}`;
  const shown = new Set(structuredBlocks.map(sourceKey));
  if (!overview.length)
    for (const block of report.documents.find((document) => document.document === "summary")?.blocks ?? [])
      shown.add(sourceKey(block));
  const bodySections = primarySections.filter((section) =>
    ["header", "cause", "fix", "limitations", "unknown"].includes(section.key),
  );
  for (const section of primarySections.filter((section) =>
    ["overview", "scope", "impact", "header", "cause", "fix", "limitations", "unknown"].includes(section.key),
  )) {
    for (const block of section.blocks) shown.add(sourceKey(block));
  }
  const remainingBlocks = report.documents
    .flatMap((document) => document.blocks)
    .filter((block) => !shown.has(sourceKey(block)));
  function proseOnly(text: string, blocks: ReportBlock[]) {
    let prose = text;
    for (const block of blocks) if (block.raw) prose = prose.replace(block.raw, "\n");
    return prose;
  }
  const coveredSources = [
    ...steps.flatMap((step) => step.sources),
    ...primarySections
      .filter((section) => !["repro", "evidence"].includes(section.key))
      .map((section) => section.source),
  ];
  // Preserve introduction/tail text that does not belong to any structured step.
  const additionalProse = report.documents
    .filter((document) => document.document !== "summary")
    .flatMap((document) => {
      const ranges = coveredSources
        .filter((source) => source.document === document.document)
        .sort((a, b) => a.start - b.start);
      const gaps: { text: string; document: string }[] = [];
      let cursor = 0;
      for (const source of ranges) {
        if (source.start > cursor)
          gaps.push({ text: document.text.slice(cursor, source.start), document: document.document });
        cursor = Math.max(cursor, source.end);
      }
      if (cursor < document.text.length) gaps.push({ text: document.text.slice(cursor), document: document.document });
      return gaps
        .map((gap) => ({
          ...gap,
          text: proseOnly(
            gap.text,
            remainingBlocks.filter((block) => block.source.document === gap.document),
          ),
        }))
        .filter((gap) => gap.text.replace(/^#{1,6}\s+[^\n]*$/gm, "").trim());
    });
  function jump(step: ReportStep, scroll = false) {
    setSelection((previous) => ({ id: step.id, revision: previous.revision + 1, scroll }));
  }
  return (
    <div className={styles.template} data-testid="template-report">
      <aside className={styles.localNav} aria-label="报告章节目录">
        <div className={styles.navTitle}>
          <FileText size={19} />
          <strong>漏洞调查</strong>
          <span>#{report.identity.id}</span>
        </div>
        <nav>
          {sectionNames.map((item) => (
            <button
              key={item.id}
              type="button"
              aria-current={activeSection === item.id ? "location" : undefined}
              onClick={() => goToSection(item.id)}
            >
              <item.icon size={17} />
              <span>{item.label}</span>
              {item.id === "evidence" && <small>{steps.length}</small>}
            </button>
          ))}
        </nav>
        <div className={styles.navContext}>
          <span>调查对象</span>
          <strong>{finding?.assets?.[0]?.label || report.identity.vulnclass || "当前漏洞"}</strong>
          <span>
            <Database size={13} />
            来自已保存的真实记录
          </span>
          <span>
            <Link2 size={13} />
            证据与原文可追溯
          </span>
        </div>
      </aside>
      <div className={styles.content} ref={contentRef}>
        <section id={sectionId("overview")} data-report-section="overview" className={styles.overview}>
          <div className={styles.verdict} data-has-impact={!!impactText.trim()}>
            <div className={styles.verdictMain}>
              <h2>
                <ShieldAlert size={23} />
                调查结论
              </h2>
              <p className={styles.verdictPreview}>
                {preview(verdictText, 180) || "此记录暂无摘要，请查看下方原始证据。"}
              </p>
              {activeStep && (
                <button type="button" className={styles.primaryAction} onClick={() => jump(activeStep, true)}>
                  检查关键证据 <ArrowDown size={15} />
                </button>
              )}
              <div className={styles.evidenceBoundary}>
                <FileText size={18} />
                <div>
                  <strong>结论来自报告记录</strong>
                  <p>尚未独立复测 · {chain ? chain.traffic_count + " 条原始流量绑定" : "流量绑定以原始流量区域为准"}</p>
                </div>
                {overview[0] && <SourceButton source={overview[0].source} onSource={onSource} />}
              </div>
            </div>
            {!!impactText.trim() && (
              <aside className={styles.impacts}>
                <h3>已记录的影响</h3>
                {impactPoints.slice(0, 3).map((impact, index) => {
                  const Icon = [UserRound, Database, Network][index];
                  return (
                    <div className={styles.impact} key={`${impact.lead}-${impact.body}`}>
                      <Icon size={18} />
                      <div>
                        <strong>{preview(impact.lead, 44) || "报告记载"}</strong>
                        <p>{preview(impact.body, 100)}</p>
                      </div>
                    </div>
                  );
                })}
                <SourceButton source={impactSections[0]?.source} onSource={onSource} />
              </aside>
            )}
          </div>
          <dl className={styles.metrics}>
            <div>
              <dt>解析到的入口路径</dt>
              <dd>{report.scope.endpoints.length}</dd>
            </div>
            <div>
              <dt>数据库绑定资产</dt>
              <dd>{finding?.assets?.length ?? 0}</dd>
            </div>
            <div>
              <dt>行为与证据记录</dt>
              <dd>{steps.length}</dd>
            </div>
            <div data-warning={chain?.traffic_count === 0}>
              <dt>原始流量绑定</dt>
              <dd>{chain?.traffic_count ?? "—"}</dd>
            </div>
          </dl>
        </section>
        {hasScope && (
          <section id={sectionId("scope")} data-report-section="scope" className={styles.section}>
            <Title eyebrow="SCOPE" title="受影响范围" />
            <div className={styles.scope}>
              <div>
                {!!report.identity.asset.trim() && (
                  <div className={styles.boundAssets}>
                    <span className={styles.listCaption}>报告记载目标</span>
                    <MarkdownText text={report.identity.asset} />
                  </div>
                )}
                {finding?.assets?.length ? (
                  <div className={styles.boundAssets}>
                    <span className={styles.listCaption}>数据库绑定资产</span>
                    {finding.assets.map((asset) => (
                      <div key={asset.id}>
                        <Globe2 size={16} />
                        <code>{asset.label}</code>
                        <span>
                          {asset.type} · #{asset.id}
                        </span>
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className={styles.muted}>暂无数据库绑定资产；以下目标为报告记载。</p>
                )}
                {scopeSections.map((section) => (
                  <MarkdownText
                    key={section.id}
                    text={section.text}
                    document={section.source.document}
                    blocks={section.blocks}
                  />
                ))}
              </div>
              {report.scope.endpoints.length > 0 && (
                <div className={styles.endpoints}>
                  <div className={styles.listCaption}>
                    接口清单 <span>{report.scope.endpoints.length}</span>
                  </div>
                  {report.scope.endpoints.map((endpoint, index) => (
                    <div className={styles.endpoint} key={`${endpoint.method}-${endpoint.path}-${index}`}>
                      <span className={styles.method}>{endpoint.method || "PATH"}</span>
                      <code>{endpoint.path}</code>
                      <SourceButton source={endpoint.provenance} onSource={onSource} />
                    </div>
                  ))}
                </div>
              )}
            </div>
          </section>
        )}
        {hasPath && (
          <section id={sectionId("path")} data-report-section="path" className={styles.pathSection}>
            <div className={styles.pathHeading}>
              <div>
                <h2>
                  <GitBranch size={21} />
                  攻击路径
                </h2>
                <p>点击行为进入对应证据 · 在左侧步骤目录切换回看</p>
              </div>
              <button
                type="button"
                className={styles.playButton}
                aria-pressed={playing}
                onClick={() => {
                  setPlaying((value) => !value);
                  setPlayIndex(-1);
                }}
              >
                {playing ? <Square size={14} /> : <Play size={14} />}
                {playing ? "停止讲解" : "逐段讲解"}
              </button>
            </div>
            <div className={styles.pathSurface}>
              <ReportPath
                model={path}
                activeId={activeStep?.id}
                playingIndex={playing ? playIndex : -1}
                onStep={(step) => jump(step, true)}
                onSource={onSource}
              />
              <div className={styles.pathFooter}>
                <span>
                  <Link2 size={13} />
                  报告原文支撑
                </span>
                <span>
                  <ShieldAlert size={13} />
                  未独立复测
                </span>
                <span>
                  {playing && playIndex >= 0
                    ? "正在讲解 " + String(playIndex + 1).padStart(2, "0") + " / " + playbackLength
                    : `路径概览 · 完整 ${steps.length} 条记录见步骤目录`}
                </span>
              </div>
            </div>
          </section>
        )}
        <section
          id={sectionId("evidence")}
          data-report-section="evidence"
          className={`${styles.section} ${styles.reader}`}
          ref={readerRef}
          aria-labelledby={readerTitleId}
        >
          <div className={styles.readerHeading}>
            <div>
              <h2 id={readerTitleId}>
                <Terminal size={21} />
                复现与证据
              </h2>
              <p>选择左侧步骤查看完整证据，随时切换回看。</p>
            </div>
            {activeStep && (
              <div className={styles.stepControls}>
                <span aria-live="polite">
                  当前步骤 <strong>{String(activeIndex + 1).padStart(2, "0")}</strong> /{" "}
                  {String(steps.length).padStart(2, "0")}
                </span>
                <button
                  type="button"
                  aria-label="上一步"
                  disabled={activeIndex === 0}
                  onClick={() => jump(steps[activeIndex - 1])}
                >
                  <ChevronLeft size={15} />
                </button>
                <button
                  type="button"
                  aria-label="下一步"
                  disabled={activeIndex === steps.length - 1}
                  onClick={() => jump(steps[activeIndex + 1])}
                >
                  <ChevronRight size={15} />
                </button>
              </div>
            )}
          </div>
          {activeStep ? (
            <div className={styles.stepLayout}>
              <div className={styles.mobileStepPicker}>
                <label>
                  当前步骤
                  <select
                    aria-label="选择复现步骤"
                    value={activeStep.id}
                    onChange={(event) => {
                      const step = steps.find((entry) => entry.id === event.target.value);
                      if (step) jump(step);
                    }}
                  >
                    {steps.map((step, index) => (
                      <option key={step.id} value={step.id}>
                        {String(index + 1).padStart(2, "0")} · {chainTitle(step)}
                      </option>
                    ))}
                  </select>
                </label>
              </div>
              <nav ref={directoryRef} className={styles.stepDirectory} aria-label="复现与证据步骤目录">
                <ol>
                  {steps.map((step, index) => (
                    <li key={step.id}>
                      <button
                        type="button"
                        data-report-step-button={step.id}
                        aria-current={step.id === activeStep.id ? "step" : undefined}
                        onClick={() => jump(step)}
                        title={step.label}
                      >
                        <span className={styles.directoryNumber}>
                          {(() => {
                            const Icon = stepAction(step).icon;
                            return <Icon size={17} />;
                          })()}
                        </span>
                        <span className={styles.directoryText}>
                          <strong>{chainTitle(step)}</strong>
                          <span>
                            {step.request.length} 请求 · {step.response.length} 响应 · {step.output.length} 片段
                          </span>
                        </span>
                      </button>
                    </li>
                  ))}
                </ol>
              </nav>
              {[activeStep].map((step) => {
                const ownComparison = step.output.find((block) => comparisonRows(block).length > 0);
                const relatedComparisons = steps
                  .filter((entry) => /消融|对照/.test(entry.label))
                  .flatMap((entry) => entry.output)
                  .filter((block) => comparisonRows(block).length > 0);
                const comparison =
                  ownComparison ??
                  (/消融|对照/.test(step.label) && relatedComparisons.length === 1 ? relatedComparisons[0] : undefined);
                return (
                  <article
                    key={step.id}
                    ref={stepRef}
                    className={styles.step}
                    data-template-step={step.id}
                    aria-label={`步骤 ${activeIndex + 1}：${step.label}`}
                    tabIndex={-1}
                  >
                    <header className={styles.stepHeading}>
                      <span className={styles.stepNumber}>STEP {String(activeIndex + 1).padStart(2, "0")}</span>
                      <h3>{step.label}</h3>
                      <span className={styles.origin}>
                        {step.origin === "merged"
                          ? "复现与证据 · 同接口归组"
                          : step.origin === "repro"
                            ? "复现记录"
                            : "证据记录"}
                      </span>
                    </header>
                    {step.sources[0] && onSource && (
                      <div className={styles.sources}>
                        <button type="button" className={styles.sourceButton} onClick={() => onSource(step.sources[0])}>
                          查看此步原文 <ArrowUpRight size={12} />
                        </button>
                      </div>
                    )}
                    {step.request.length > 0 && step.response.length === 0 && (
                      <div className={styles.responseBoundary}>
                        <ShieldAlert size={16} />
                        <p>此步骤没有可识别的原始 HTTP 响应；以下输出与说明按报告原文保留。</p>
                      </div>
                    )}
                    {comparison && (
                      <div className={styles.comparisonSection}>
                        <h4>{ownComparison ? "条件与结果对照" : "关联对照记录 · 同一漏洞报告"}</h4>
                        <ReportComparison
                          key={`${comparison.source.document}-${comparison.source.line}`}
                          block={comparison}
                        />
                        <DisclosureSection
                          eyebrow="COMPARISON SOURCE"
                          title="原始对照输出"
                          detail="完整保留报告中的条件与结果"
                        >
                          <CodePanel block={comparison} onSource={onSource} />
                        </DisclosureSection>
                      </div>
                    )}
                    {step.notes.map((note, noteIndex) => (
                      <div key={`${noteIndex}-${note}`} className={styles.stepNote}>
                        <MarkdownText
                          text={proseOnly(note, [...step.request, ...step.response, ...step.output, ...step.other])}
                          document={step.sources[noteIndex]?.document ?? "report"}
                        />
                      </div>
                    ))}
                    <div className={balancedColumns(step) ? styles.paired : styles.stacked}>
                      <Pane title="请求 / REQUEST" blocks={step.request} onSource={onSource} />
                      <Pane title="响应 / RESPONSE" blocks={step.response} onSource={onSource} />
                    </div>
                    <Pane
                      title="代码与输出 / CODE & OUTPUT"
                      blocks={step.output.filter((block) => block !== comparison)}
                      onSource={onSource}
                    />
                    <Pane
                      title="补充代码与记录"
                      blocks={step.other.filter(
                        (block) => !step.output.some((output) => sourceKey(output) === sourceKey(block)),
                      )}
                      onSource={onSource}
                    />
                    {!step.request.length &&
                      !step.response.length &&
                      !step.output.length &&
                      !step.other.length &&
                      !step.notes.length &&
                      step.sources.map((source, sourceIndex) => (
                        <MarkdownText
                          key={`${source.document}-${source.line}-${sourceIndex}`}
                          text={source.text}
                          document={source.document}
                        />
                      ))}
                  </article>
                );
              })}
            </div>
          ) : (
            <div className={styles.empty}>此记录暂无可结构化的步骤；原始文档内容保留在下方章节。</div>
          )}
        </section>
        {traffic && (
          <section id={sectionId("traffic")} data-report-section="traffic" className={styles.traffic}>
            {traffic}
          </section>
        )}
        <section id={sectionId("disposition")} data-report-section="disposition" className={styles.disposition}>
          <div className={styles.dispositionHeading}>
            <h2>
              <Wrench size={21} />
              PoC 与处置
            </h2>
            <p>验证脚本、根因和修复建议按需展开。</p>
          </div>
          {languages.length > 0 && (
            <div className={styles.tools}>
              <span>报告中的材料</span>
              {languages.map((language) => (
                <span key={language}>
                  {/curl|shell|bash|powershell/i.test(language) ? (
                    <Terminal size={15} />
                  ) : /http/i.test(language) ? (
                    <Network size={15} />
                  ) : /json/i.test(language) ? (
                    <Database size={15} />
                  ) : (
                    <FileCode2 size={15} />
                  )}
                  {language}
                </span>
              ))}
            </div>
          )}
          {(report.poc.intro || report.poc.main || report.poc.extras.length > 0) && (
            <DisclosureSection
              eyebrow="PROOF OF CONCEPT"
              title="PoC 与验证脚本"
              detail={`${pocBlocks.length} 个片段 · ${pocLines} 行`}
              collapsible={longPoc}
            >
              <div className={styles.sectionBody}>
                <MarkdownText
                  text={proseOnly(report.poc.intro, [
                    ...(report.poc.main ? [report.poc.main] : []),
                    ...report.poc.extras,
                  ])}
                />
                {report.poc.main && <CodePanel block={report.poc.main} onSource={onSource} />}
                {report.poc.extras.map((block, index) => (
                  <CodePanel
                    key={`${block.source.document}-${block.source.line}-${index}`}
                    block={block}
                    onSource={onSource}
                  />
                ))}
              </div>
            </DisclosureSection>
          )}
          {references.length > 0 && (
            <DisclosureSection
              eyebrow="RELATED EVIDENCE"
              title="关联漏洞与探索记录"
              detail="探索节点与独立漏洞分别标识；引用不自动认定为利用依赖。"
            >
              <Title
                eyebrow="RELATED EVIDENCE"
                title="关联漏洞与探索记录"
                detail="来自报告明确引用，探索节点与独立漏洞编号分别标识；引用不自动认定为利用依赖。"
              />
              <div className={styles.relations}>
                {references.map((node) => (
                  <div
                    className={styles.relation}
                    key={node.id}
                    data-reference={node.reference}
                    data-finding-id={node.finding_id ?? ""}
                  >
                    <span className={styles.reference}>
                      <GitBranch size={13} />
                      {node.reference}
                    </span>
                    <div>
                      <h3>{node.title}</h3>
                      <p>{node.summary}</p>
                      {node.finding_id && (
                        <span className={styles.relationMeta}>探索节点 → 漏洞 #{node.finding_id}</span>
                      )}
                    </div>
                    <div className={styles.relationActions}>
                      {node.finding_id &&
                        /^\d+$/.test(node.finding_id) &&
                        (onRelated ? (
                          <button
                            type="button"
                            className={styles.relatedButton}
                            onClick={() => onRelated(node.finding_id!)}
                          >
                            查看漏洞 #{node.finding_id}
                            <ArrowUpRight size={12} />
                          </button>
                        ) : (
                          <Link
                            className={styles.relatedButton}
                            href={`/function/findings/detail/?id=${encodeURIComponent(node.finding_id)}${contextTaskId ? `&context_task=${encodeURIComponent(contextTaskId)}` : ""}`}
                          >
                            查看漏洞 #{node.finding_id}
                            <ArrowUpRight size={12} />
                          </Link>
                        ))}
                      {onReference && chain?.sources.find((source) => node.sources.includes(source.id)) && (
                        <button
                          type="button"
                          className={styles.sourceButton}
                          onClick={() => {
                            const source = chain?.sources.find((entry) => node.sources.includes(entry.id));
                            if (source) onReference(source);
                          }}
                        >
                          查看探索原文
                          <ArrowUpRight size={11} />
                        </button>
                      )}
                    </div>
                  </div>
                ))}
              </div>
            </DisclosureSection>
          )}
          {bodySections.map((section) => (
            <DisclosureSection
              key={`${section.source.document}-${section.id}`}
              eyebrow="ORIGINAL SECTION"
              title={section.heading || "调查元数据与原始说明"}
            >
              <div className={styles.sectionBody}>
                <SourceButton source={section.source} onSource={onSource} />
                <MarkdownText
                  text={section.key === "header" ? section.text.replace(/^#\s+[^\n]*(?:\n|$)/m, "") : section.text}
                  document={section.source.document}
                  blocks={section.blocks}
                />
              </div>
            </DisclosureSection>
          ))}
          {additionalProse.length > 0 && (
            <DisclosureSection eyebrow="ADDITIONAL CONTEXT" title="补充原始说明">
              <div className={styles.sectionBody}>
                {additionalProse.map((part, index) => (
                  <MarkdownText key={`${part.document}-${index}`} text={part.text} document={part.document} />
                ))}
              </div>
            </DisclosureSection>
          )}
          {remainingBlocks.length > 0 && (
            <DisclosureSection
              eyebrow="ADDITIONAL EVIDENCE"
              title="其他原始片段"
              detail={`${remainingBlocks.length} 个代码与输出片段`}
            >
              <div className={styles.sectionBody}>
                {remainingBlocks.map((block) => (
                  <CodePanel key={sourceKey(block)} block={block} onSource={onSource} />
                ))}
              </div>
            </DisclosureSection>
          )}
        </section>
        {report.verification.reportStale && (
          <p className={styles.notice}>报告依据的证据版本已变更；当前展示已保存记录，请结合下方最新绑定流量核对。</p>
        )}
      </div>
    </div>
  );
}
