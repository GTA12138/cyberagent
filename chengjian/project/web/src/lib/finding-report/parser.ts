import type {
  BlockKind,
  FindingReport,
  FindingReportInput,
  MergedFindingReport,
  ReportBlock,
  ReportDocument,
  ReportEndpoint,
  ReportGroup,
  ReportReferences,
  ReportSection,
  ReportSource,
  ReportStep,
  ReportVerify,
  RequestTemplate,
} from "./types";

export type * from "./types";

interface Line {
  text: string;
  start: number;
  end: number;
  n: number;
}
interface Fence {
  char: string;
  length: number;
}
const METHODS = "GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS|CONNECT|TRACE";
const SECTION_ALIASES: [string, RegExp][] = [
  ["overview", /^(概述|漏洞概述|摘要|概览)$/],
  ["impact", /^(影响与危害|危害|安全影响|影响分析|风险评估|影响范围)$/],
  ["scope", /^(受影响范围|影响资产|受影响资产)$/],
  ["repro", /^(复现步骤|复现过程|验证过程|利用过程|复现与验证)$/],
  ["evidence", /^(证据|验证证据|证据记录)$/],
  ["poc", /^(poc|proof of concept|验证脚本)$/i],
  ["cause", /^(根因分析|根因|原因分析|漏洞原理)$/],
  ["fix", /^(修复建议|修复方案|缓解措施)$/],
  ["limitations", /未验证|待确认|需进一步确认|局限|限制|边界|^备注$/],
];

function linesOf(text: string): Line[] {
  const lines: Line[] = [];
  let start = 0;
  for (const value of text.matchAll(/[^\n]*(?:\n|$)/g)) {
    if (!value[0]) continue;
    const raw = value[0];
    lines.push({ text: raw.replace(/\r?\n$/, ""), start, end: start + raw.length, n: lines.length + 1 });
    start += raw.length;
  }
  return lines;
}
function openingFence(line: string): { fence: Fence; lang: string } | null {
  const match = /^ {0,3}(`{3,}|~{3,})([^\r\n]*)$/.exec(line);
  if (!match || (match[1][0] === "`" && match[2].includes("`"))) return null;
  return { fence: { char: match[1][0], length: match[1].length }, lang: match[2].trim().split(/\s/)[0].toLowerCase() };
}
function closesFence(line: string, fence: Fence): boolean {
  const match = /^ {0,3}(`+|~+)\s*$/.exec(line);
  return !!match && match[1][0] === fence.char && match[1].length >= fence.length;
}
function range(
  text: string,
  document: ReportSource["document"],
  findingId: string,
  start: number,
  end: number,
): ReportSource {
  return {
    findingId,
    document,
    start,
    end,
    line: text.slice(0, start).split("\n").length,
    endLine: text.slice(0, Math.max(start, end - 1)).split("\n").length,
    text: text.slice(start, end),
  };
}
function subrange(source: ReportSource, text: string, start: number, end: number): ReportSource {
  const before = text.slice(0, start);
  return {
    ...source,
    start: source.start + start,
    end: source.start + end,
    line: source.line + before.split("\n").length - 1,
    endLine: source.line + text.slice(0, Math.max(start, end - 1)).split("\n").length - 1,
    text: text.slice(start, end),
  };
}
export function isUpstreamExcerpt(text: string): boolean {
  return /\.\.\.|…|⋯|\[(?:省略|截断|omitted|truncated)[^\]]*\]|(?:内容|响应|输出).{0,8}(?:已截断|省略)|\b(?:truncated|omitted)\b/i.test(
    text,
  );
}
export function classifyBlock(body: string, lang = ""): BlockKind {
  const text = body.trim();
  if (new RegExp(`^(?:${METHODS})\\s+\\S+\\s+HTTP/[\\d.]+`, "i").test(text)) return "http-request";
  if (/^curl(?:\.exe)?(?:\s|$)/i.test(text)) return "curl";
  if (/^HTTP\/[\d.]+\s+\d{3}\b/i.test(text)) return "http-response";
  if (/^[A-Za-z][A-Za-z-]+:\s/.test(text) && text.includes("\n")) return "http-headers";
  if (/^(python|py|javascript|js|typescript|ts|go|java|ruby|php|c|cpp)$/i.test(lang)) return "code";
  if (/^(json|jsonc)$/i.test(lang)) return "json";
  if (/^(bash|sh|shell|powershell|ps1|cmd|bat)$/i.test(lang)) return "shell";
  if (/^(def |import |class |package |using |function |#!)/.test(text)) return "code";
  if (/^[[{]/.test(text)) return "json";
  return "output";
}

/** Recognizes variable-length backtick/tilde fences; incomplete blocks retain all remaining text. */
export function extractBlocks(text: string, source?: ReportSource): ReportBlock[] {
  const base = source ?? range(text, "report", "", 0, text.length);
  const lines = linesOf(text);
  const blocks: ReportBlock[] = [];
  for (let i = 0; i < lines.length; i++) {
    const opening = openingFence(lines[i].text);
    if (!opening) continue;
    let closing = i + 1;
    while (closing < lines.length && !closesFence(lines[closing].text, opening.fence)) closing++;
    const closed = closing < lines.length;
    const end = closed ? lines[closing].end : text.length;
    const bodyEnd = closed ? lines[closing].start : text.length;
    const body = text.slice(lines[i].end, bodyEnd);
    const excerpted = isUpstreamExcerpt(body);
    blocks.push({
      lang: opening.lang,
      body,
      kind: classifyBlock(body, opening.lang),
      index: blocks.length,
      source: subrange(base, text, lines[i].start, end),
      raw: text.slice(lines[i].start, end),
      closed,
      excerpted,
      warnings: [
        ...(!closed ? ["原始代码围栏未闭合，已保留至文档结尾。"] : []),
        ...(excerpted ? ["原文含省略或截断标记，属于上游摘录；未补造缺失内容。"] : []),
      ],
    });
    i = closed ? closing : lines.length;
  }
  return blocks;
}
function sectionKey(heading: string): string {
  const clean = heading.replace(/\*\*/g, "").trim();
  return SECTION_ALIASES.find(([, pattern]) => pattern.test(clean))?.[0] ?? "unknown";
}
/** Top-level sections are partitioned outside fences; every original byte belongs to one section. */
function parseDocument(text: string, document: ReportSource["document"], findingId: string): ReportDocument {
  const lines = linesOf(text);
  const headings: { start: number; body: number; heading: string; level: number }[] = [];
  let fence: Fence | null = null;
  for (const line of lines) {
    if (fence) {
      if (closesFence(line.text, fence)) fence = null;
      continue;
    }
    const opening = openingFence(line.text);
    if (opening) {
      fence = opening.fence;
      continue;
    }
    const heading = /^ {0,3}(#{1,6})[\t ]+(.+?)(?:[\t ]+#+[\t ]*)?$/.exec(line.text);
    if (heading && (heading[1].length === 2 || sectionKey(heading[2]) !== "unknown")) {
      headings.push({ start: line.start, body: line.end, heading: heading[2], level: heading[1].length });
    }
  }
  const sections: ReportSection[] = [];
  const add = (start: number, end: number, body: number, heading: string, level: number) => {
    if (start === end) return;
    const source = range(text, document, findingId, start, end);
    const contentSource = range(text, document, findingId, body, end);
    sections.push({
      id: `${document}-${sections.length + 1}`,
      heading,
      key: heading ? sectionKey(heading) : "header",
      level,
      raw: text.slice(start, end),
      text: text.slice(body, end),
      source,
      blocks: extractBlocks(text.slice(body, end), contentSource),
    });
  };
  let start = 0;
  let body = 0;
  let title = "";
  let level = 0;
  for (const heading of headings) {
    add(start, heading.start, body, title, level);
    start = heading.start;
    body = heading.body;
    title = heading.heading;
    level = heading.level;
  }
  add(start, text.length, body, title, level);
  return { document, text, sections, blocks: extractBlocks(text, range(text, document, findingId, 0, text.length)) };
}
function headingLabel(line: string): string | null {
  const text = line.trim();
  const markdown = /^#{3,6}\s+(.+)$/.exec(text);
  if (markdown) return markdown[1];
  const bold = /^(?:\d+[.)、]\s*)?\*\*(.+)\*\*\s*[：:]?\s*$/.exec(text);
  if (bold && !/^[\d\s/.×→%=<>~-]+$/.test(bold[1])) return bold[1];
  const numbered =
    /^(?:[①②③④⑤⑥⑦⑧⑨⑩⑪⑫⑬⑭⑮⑯⑰⑱⑲⑳]\s*|步骤\s*[一二三四五六七八九十\d]+\s*[：:、.．—-]?\s*|\d+[.)、]\s+)(.+)$/.exec(text);
  return numbered?.[1] ?? null;
}
export function extractRefs(text: string): ReportReferences {
  const kinds: Record<string, Set<number>> = { finding: new Set(), fact: new Set(), intent: new Set() };
  const pattern = /\b(finding|fact|intent)[ _#：:]*([0-9]+(?:\s*[/、,，]\s*[0-9]+)*)/gi;
  for (const match of text.matchAll(pattern)) {
    for (const number of match[2].matchAll(/\d+/g)) {
      const id = Number(number[0]);
      if (Number.isSafeInteger(id) && id > 0) kinds[match[1].toLowerCase()].add(id);
    }
  }
  return {
    findings: [...kinds.finding].sort((a, b) => a - b),
    facts: [...kinds.fact].sort((a, b) => a - b),
    intents: [...kinds.intent].sort((a, b) => a - b),
    namespace: "exploration-node",
  };
}
export function extractEndpoints(text: string, source?: ReportSource): ReportEndpoint[] {
  const base = source ?? range(text, "report", "", 0, text.length);
  const output: ReportEndpoint[] = [];
  const seen = new Set<string>();
  const push = (method: string, path: string, start: number, end: number, kind: ReportEndpoint["source"]) => {
    const key = `${method} ${path}`;
    if (seen.has(key)) return;
    seen.add(key);
    output.push({ method, path, source: kind, provenance: subrange(base, text, start, end) });
  };
  for (const match of text.matchAll(
    new RegExp(`\\b(${METHODS})\\s+(https?://[^\\s\\x60<>"']+|/[^\\s\\x60<>"']+)`, "g"),
  )) {
    const target = match[2].replace(/[，。；,)）]+$/, "");
    push(match[1], target, match.index, match.index + match[0].length, "explicit");
  }
  for (const match of text.matchAll(/`(\/[^`\s]+)`/g))
    push("", match[1], match.index, match.index + match[0].length, "path");
  return output;
}
function groupsForSection(section: ReportSection, document: ReportDocument): ReportGroup[] {
  const contentStart = section.source.end - section.text.length;
  const contentSource = range(
    document.text,
    document.document,
    section.source.findingId,
    contentStart,
    section.source.end,
  );
  return groupsForText(section.text, contentSource, section.heading || "原始记录");
}
function groupsForText(text: string, source: ReportSource, fallback: string): ReportGroup[] {
  if (!text) return [];
  const lines = linesOf(text);
  const marks: { start: number; label: string }[] = [];
  let fence: Fence | null = null;
  for (const line of lines) {
    if (fence) {
      if (closesFence(line.text, fence)) fence = null;
      continue;
    }
    const opening = openingFence(line.text);
    if (opening) {
      fence = opening.fence;
      continue;
    }
    const label = headingLabel(line.text);
    if (label) marks.push({ start: line.start, label });
  }
  if (!marks.length) marks.push({ start: 0, label: fallback });
  const preamble = text.slice(0, marks[0].start);
  return marks.map((mark, index) => {
    const start = index === 0 ? 0 : mark.start;
    const end = marks[index + 1]?.start ?? text.length;
    const raw = text.slice(start, end);
    const groupSource = subrange(source, text, start, end);
    return {
      n: index + 1,
      label: mark.label,
      text: raw,
      raw,
      ...(index === 0 && preamble ? { preamble } : {}),
      source: groupSource,
      blocks: extractBlocks(raw, groupSource),
      refs: extractRefs(raw),
      endpoints: extractEndpoints(raw, groupSource),
    };
  });
}
function textWithoutBlocks(text: string, blocks: ReportBlock[]): string {
  let output = text;
  for (const block of blocks) output = output.replace(block.raw, "\n");
  return output;
}
function allParagraphs(text: string): string[] {
  return text.split(/\r?\n[\t ]*\r?\n/).filter((value) => value.trim());
}
function listItems(text: string): string[] {
  const lines = linesOf(text);
  const output: string[] = [];
  let current = "";
  let fence: Fence | null = null;
  for (const line of lines) {
    const opening: ReturnType<typeof openingFence> = !fence ? openingFence(line.text) : null;
    const marker = !fence && !opening ? /^\s*(?:[-*+]\s+|\d+[.)、]\s+)(.*)$/.exec(line.text) : null;
    if (marker) {
      if (current.trim()) output.push(current);
      current = marker[1];
    } else current += (current ? "\n" : "") + line.text;
    if (fence && closesFence(line.text, fence)) fence = null;
    else if (opening) fence = opening.fence;
  }
  if (current.trim()) output.push(current);
  return output;
}
function splitLead(text: string): { lead: string; body: string } {
  const match = /^\*\*([^*]+)\*\*\s*[：:—–-]?\s*([\s\S]*)$/.exec(text.trim());
  return match ? { lead: match[1], body: match[2] } : { lead: "", body: text };
}
function nonempty(...values: (string | undefined)[]): string {
  return values.find((value) => !!value?.trim()) ?? "";
}
function parseHeader(document: ReportDocument) {
  const head = document.sections.find((section) => section.key === "header")?.raw ?? "";
  const clean = textWithoutBlocks(head, extractBlocks(head));
  const meta: Record<string, string> = {};
  const key = /(?:^|[　\s|｜])(?:\*\*)?([\p{L}#][\p{L}\d #/]{0,30}?)(?:\*\*)?\s*[：:]\s*/gu;
  for (const raw of clean.split("\n")) {
    const line = raw.trim().replace(/^(?:[-*]\s+|>\s?)/, "");
    if (!line || line.startsWith("#")) continue;
    const marks = [...line.matchAll(key)];
    marks.forEach((mark, index) => {
      const k = mark[1].trim();
      const value = line.slice(mark.index + mark[0].length, marks[index + 1]?.index ?? line.length).trim();
      if (value) meta[k] = meta[k] ? `${meta[k]}\n${value}` : value;
    });
  }
  const pick = (pattern: RegExp) => Object.entries(meta).find(([k]) => pattern.test(k))?.[1] ?? "";
  const severityText = pick(/严重|等级|severity/i).toLowerCase();
  const severity =
    (
      [
        ["critical", /\bcritical\b|严重|超危/],
        ["high", /\bhigh\b|高危|^高$/],
        ["medium", /\bmedium\b|中危|^中$/],
        ["low", /\blow\b|低危|^低$/],
      ] as const
    ).find(([, pattern]) => pattern.test(severityText))?.[0] ?? "";
  return {
    title:
      /^ {0,3}#\s+(.+)$/m
        .exec(clean)?.[1]
        .replace(/^漏洞报告[：:]\s*/, "")
        .trim() ?? "",
    severity,
    vulnclass: pick(/漏洞类型|漏洞类别|类型/),
    asset: pick(/资产|目标|主机/),
    intent: pick(/意图|work#|验证方式/),
    meta,
  };
}

export function normalizeReport(finding: FindingReportInput): FindingReport;
export function normalizeReport(
  id: string | number,
  markdown: string,
  db?: Omit<FindingReportInput, "id">,
): FindingReport;
export function normalizeReport(
  input: FindingReportInput | string | number,
  markdown = "",
  db: Omit<FindingReportInput, "id"> = {},
): FindingReport {
  const finding: FindingReportInput = typeof input === "object" ? input : { ...db, id: input, report: markdown };
  const id = String(finding.id);
  const raw = { report: finding.report ?? "", evidence: finding.evidence ?? "", summary: finding.summary ?? "" };
  const documents: ReportDocument[] = [
    parseDocument(raw.report, "report", id),
    parseDocument(raw.evidence, "evidence", id),
    parseDocument(raw.summary, "summary", id),
  ];
  const reportDoc = documents[0],
    header = parseHeader(reportDoc),
    warnings: string[] = [];
  const byKey = (key: string) => reportDoc.sections.filter((section) => section.key === key);
  const sectionText = (key: string) =>
    byKey(key)
      .map((section) => section.text)
      .join("\n");
  const sectionBlocks = (key: string) => byKey(key).flatMap((section) => section.blocks);
  const sections = documents.flatMap((document) => document.sections);
  const groups = (key: string) => byKey(key).flatMap((section) => groupsForSection(section, reportDoc));
  if (finding.severity && header.severity && finding.severity !== header.severity)
    warnings.push(`严重等级不一致：报告头部=${header.severity}，数据库列=${finding.severity}（采用数据库）`);
  if (!raw.report.trim()) warnings.push("报告正文为空，独立证据和摘要仍完整保留。");
  if (!header.title && finding.name) warnings.push("报告缺少H1标题，采用数据库name。");
  const allBlocks = documents.flatMap((document) => document.blocks);
  for (const block of allBlocks)
    for (const warning of block.warnings) warnings.push(`${block.source.document} L${block.source.line}：${warning}`);
  const overview = sectionText("overview") || raw.summary;
  const impact = sectionText("impact"),
    scope = sectionText("scope"),
    repro = sectionText("repro"),
    cause = sectionText("cause"),
    fix = sectionText("fix"),
    limits = byKey("limitations");
  const evidenceGroups = groups("evidence");
  if (raw.evidence)
    evidenceGroups.push(
      ...groupsForText(raw.evidence, range(raw.evidence, "evidence", id, 0, raw.evidence.length), "独立证据 / PoC"),
    );
  const pocBlocks = sectionBlocks("poc");
  const pocMain = pocBlocks.reduce<ReportBlock | null>(
    (best, block) => (!best || block.body.length > best.body.length ? block : best),
    null,
  );
  const endpoints = documents.flatMap((document) =>
    extractEndpoints(document.text, range(document.text, document.document, id, 0, document.text.length)),
  );
  const reportStale =
    !!finding.report_stale ||
    (finding.evidence_version !== undefined &&
      finding.report_evidence_version !== undefined &&
      finding.report_evidence_version < finding.evidence_version);
  if (reportStale) warnings.push("报告对应的证据版本早于当前版本，需核对最新证据。");
  return {
    schema: "chengjian.finding.report/v1",
    source: { findingId: finding.id, format: "legacy-md", chars: raw.report.length },
    identity: {
      id: finding.id,
      title: nonempty(finding.name, header.title, `Finding #${id}`),
      severity: nonempty(finding.severity, header.severity, "unknown"),
      vulnclass: nonempty(finding.vulnclass, header.vulnclass, "未分类"),
      asset: header.asset,
      intent: header.intent,
      status: nonempty(finding.status, "pending"),
      headerLines: Object.entries(header.meta).map(([k, v]) => ({ k, v })),
      warnings,
    },
    verdict: { statement: overview, points: listItems(overview) },
    impacts: listItems(impact).map(splitLead),
    impactProse: allParagraphs(impact),
    scope: { bullets: listItems(scope), endpoints },
    reproduction: {
      groups: groups("repro"),
      numbered: listItems(repro),
      blocks: sectionBlocks("repro"),
      precondition: repro
        .split("\n")
        .filter((line) => /^\s*>/.test(line))
        .join("\n"),
      preamble: groups("repro")
        .map((group) => group.preamble ?? "")
        .filter(Boolean)
        .join("\n"),
    },
    evidence: { groups: evidenceGroups, blocks: [...sectionBlocks("evidence"), ...documents[1].blocks] },
    poc: { intro: sectionText("poc"), main: pocMain, extras: pocBlocks.filter((block) => block !== pocMain) },
    rootCause: listItems(cause),
    remediation: listItems(fix),
    limitations: {
      heading: limits.map((section) => section.heading).join(" / ") || null,
      items: limits.flatMap((section) => listItems(section.text)),
    },
    relations: extractRefs(Object.values(raw).join("\n")),
    metrics: {
      chars: Object.values(raw).reduce((sum, text) => sum + text.length, 0),
      codeBlocks: allBlocks.length,
      endpoints: endpoints.length,
      steps: evidenceGroups.length,
      hasTable: /^\s*\|?\s*:?-{3}/m.test(raw.report),
    },
    documents,
    sections,
    unrecognizedSections: sections.filter((section) => section.key === "unknown"),
    rawMarkdown: raw.report,
    raw,
    verification: {
      status: "reported-only",
      verified: false,
      reportStale,
      note: "此视图仅重组已存报告与证据。出现响应或成功状态码不代表系统已独立复测或证实漏洞。",
    },
  };
}

function direction(block: ReportBlock): "request" | "response" | "other" {
  if (block.kind === "http-request" || block.kind === "curl") return "request";
  if (block.kind === "http-response") return "response";
  if (
    (block.kind === "shell" || block.kind === "output") &&
    /(^|\n)\s*(?:curl(?:\.exe)?|wget|python3?|node|bash|sh|Invoke-WebRequest|iwr)\b/i.test(block.body)
  )
    return "request";
  // JSON is evidence/output, not automatically an HTTP response.
  return "other";
}
function groupStep(group: ReportGroup, origin: "repro" | "evidence"): ReportStep {
  const request = group.blocks.filter((block) => direction(block) === "request"),
    response = group.blocks.filter((block) => direction(block) === "response"),
    other = group.blocks.filter((block) => direction(block) === "other");
  return {
    id: "",
    label: group.label,
    origin,
    request,
    response,
    other,
    output: other,
    notes: [group.text],
    sig: group.endpoints
      .filter((endpoint) => !!endpoint.method)
      .map((endpoint) => `${endpoint.method} ${endpoint.path}`),
    sources: [group.source],
    match: "none",
    verification: "reported-only",
  };
}
/** Merge only unique exact method+target matches; ambiguous evidence remains a separate, complete record. */
export function deriveSteps(report: FindingReport): ReportStep[] {
  const repro = report.reproduction.groups.map((group) => groupStep(group, "repro"));
  const evidence = report.evidence.groups.map((group) => groupStep(group, "evidence"));
  const used = new Set<number>();
  const result = repro.map((step) => {
    const matches = evidence
      .map((candidate, index) => ({ candidate, index }))
      .filter(({ candidate, index }) => !used.has(index) && step.sig.some((sig) => candidate.sig.includes(sig)));
    if (matches.length !== 1) return step;
    const { candidate, index } = matches[0];
    // A unique candidate must also point to only one reproduction group.
    if (repro.filter((other) => other.sig.some((sig) => candidate.sig.includes(sig))).length !== 1) return step;
    used.add(index);
    return {
      ...step,
      origin: "merged" as const,
      request: [...step.request, ...candidate.request],
      response: [...step.response, ...candidate.response],
      other: [...step.other, ...candidate.other],
      output: [...step.output, ...candidate.output],
      notes: [...step.notes, ...candidate.notes],
      sig: [...new Set([...step.sig, ...candidate.sig])],
      sources: [...step.sources, ...candidate.sources],
      evidenceLabel: candidate.label,
      match: "exact-endpoint" as const,
    };
  });
  result.push(...evidence.filter((_, index) => !used.has(index)));
  return result.map((step, index) => ({ ...step, id: `step-${index + 1}` }));
}

function emptyTemplate(): RequestTemplate {
  return {
    method: "",
    path: "",
    host: "",
    scheme: "",
    protocol: "",
    headers: [],
    body: "",
    source: "none",
    provenance: [],
    missing: [],
    complete: false,
    excerpted: false,
  };
}
function requestTemplate(block: ReportBlock): RequestTemplate {
  const template = emptyTemplate();
  template.provenance = [block.source];
  template.excerpted = block.excerpted;
  if (block.kind === "http-request") {
    template.source = "report-http-block";
    const lines = block.body.replace(/\r\n/g, "\n").trimStart().split("\n");
    const first = block.body.trimStart().split(/\r?\n/)[0];
    const match = new RegExp(`^(${METHODS})\\s+(\\S+)\\s+(HTTP/[\\d.]+)\\s*$`, "i").exec(first);
    // biome-ignore lint/suspicious/noUnnecessaryConditions: Keep malformed stored requests readable even if classification changes.
    if (match) {
      template.method = match[1];
      template.path = match[2];
      template.protocol = match[3];
    }
    let bodyAt = -1;
    for (let i = 1; i < lines.length; i++) {
      if (lines[i] === "") {
        bodyAt = i + 1;
        break;
      }
      const header = /^([^:\s]+):[\t ]*(.*)$/.exec(lines[i]);
      if (header) {
        template.headers.push([header[1], header[2]]);
        if (/^host$/i.test(header[1])) template.host = header[2];
      }
    }
    if (bodyAt >= 0) template.body = lines.slice(bodyAt).join("\n");
    if (/^https?:\/\//i.test(template.path))
      try {
        const url = new URL(template.path);
        template.scheme = url.protocol.slice(0, -1);
        template.host ||= url.host;
      } catch {
        /* Preserve malformed source verbatim. */
      }
  } else {
    template.source = "report-curl";
    // Extract only explicit options. Curl defaults and shell substitutions are never guessed.
    const text = block.body;
    const method = /(?:^|\s)(?:-X\s*|--request(?:\s+|=))(?:['"])?([A-Z]+)\b/.exec(text);
    template.method = method?.[1] ?? "";
    const urlMatch = /(?:^|\s|['"])(https?:\/\/[^\s'"\\]+)/.exec(text);
    if (urlMatch)
      try {
        const url = new URL(urlMatch[1]);
        template.scheme = url.protocol.slice(0, -1);
        template.host = url.host;
        template.path = url.pathname + url.search;
      } catch {
        /* No guessed URL. */
      }
    for (const header of text.matchAll(/(?:^|\s)(?:-H|--header)\s+(['"])([^\n]*?)\1/g)) {
      const colon = header[2].indexOf(":");
      if (colon >= 0) template.headers.push([header[2].slice(0, colon), header[2].slice(colon + 1).trimStart()]);
    }
    const data = /(?:^|\s)(?:--data|--data-raw|--data-binary|-d)\s+(['"])([\s\S]*?)\1/.exec(text);
    template.body = data?.[2] ?? "";
  }
  template.missing = [
    ...(!template.method ? ["method"] : []),
    ...(!template.path ? ["request-target"] : []),
    ...(!template.host ? ["host"] : []),
  ];
  template.complete = template.missing.length === 0 && !template.excerpted;
  return template;
}
function shellQuote(text: string): string {
  return `'${text.replace(/'/g, `'\\''`)}'`;
}
export function toCurl(template: RequestTemplate): string {
  let target = "";
  if (/^https?:\/\//i.test(template.path)) target = template.path;
  else if (template.scheme && template.host) target = `${template.scheme}://${template.host}${template.path}`;
  if (!template.method || !target) return "";
  const lines = [
    "# 派生文本：仅重排报告明确字段；未执行或验证。",
    `curl --request ${shellQuote(template.method)} ${shellQuote(target)}`,
  ];
  for (const [name, value] of template.headers) lines[1] += ` \\\n  --header ${shellQuote(`${name}: ${value}`)}`;
  if (template.body) lines[1] += ` \\\n  --data-raw ${shellQuote(template.body)}`;
  return lines.join("\n");
}
export function toRawHttp(template: RequestTemplate): string {
  if (!template.method || !template.path) return "";
  // The HTTP version and additional headers are absent unless explicitly recorded.
  const lines = [
    "# 派生 HTTP 片段：未推测协议版本、Host 或其他头部。",
    `${template.method} ${template.path}${template.protocol ? ` ${template.protocol}` : ""}`,
  ];
  for (const [name, value] of template.headers) lines.push(`${name}: ${value}`);
  return lines.join("\n") + (template.body ? `\n\n${template.body}` : "");
}
function copyKind(original: boolean, derived: boolean): ReportVerify["curlKind"] {
  if (original) return "original";
  if (derived) return "derived";
  return "unavailable";
}
export function buildVerify(report: FindingReport): ReportVerify {
  const blocks = report.documents.flatMap((document) => document.blocks);
  const primary = blocks.find((block) => block.kind === "http-request" || block.kind === "curl");
  // Both copy formats must come from this exact source, never from unrelated steps.
  const http = primary?.kind === "http-request" ? primary : undefined;
  const curl = primary?.kind === "curl" ? primary : undefined;
  const template = primary ? requestTemplate(primary) : emptyTemplate();
  if (!primary) {
    const endpoint = report.scope.endpoints.find((value) => !!value.method) ?? report.scope.endpoints[0];
    if (endpoint) {
      template.source = "scope-only";
      template.method = endpoint.method;
      template.path = endpoint.path;
      template.provenance = [endpoint.provenance];
    }
    template.missing = [
      ...(!template.method ? ["method"] : []),
      ...(!template.path ? ["request-target"] : []),
      "request-headers",
      "body-unknown",
    ];
  }
  const derivedCurl = !curl ? toCurl(template) : "";
  // Curl is retained verbatim. General shell semantics cannot be reconstructed safely
  // by a regex, so it does not become a synthesized raw request.
  const derivedRaw = "";
  const incomplete = !template.complete || !http || template.excerpted;
  return {
    template,
    curl: curl?.body ?? derivedCurl,
    raw: http?.body ?? derivedRaw,
    curlKind: copyKind(!!curl, !!derivedCurl),
    rawKind: copyKind(!!http, !!derivedRaw),
    sources: [
      ...(http ? [http.source] : []),
      ...(curl ? [curl.source] : []),
      ...(!http && !curl ? template.provenance : []),
    ],
    incomplete,
    confidence: primary ? "documented" : "low",
    confidenceNote: primary
      ? "来自已存报告的请求或命令记录；没有自动执行，不代表参数完整、可直接重放或漏洞已复测。"
      : "未找到原始请求；仅保留明确接口线索，不猜测请求字段。",
    burpNote: "仅复制并审阅已记录报文。派生片段或缺失字段需人工核对；此视图不发送请求。",
    warnings: [
      ...(incomplete
        ? [`数据包不完整或未确认完整性${template.missing.length ? `：${template.missing.join("、")}` : ""}。`]
        : []),
      ...(template.excerpted ? ["上游原文包含省略标记，不能据此恢复完整响应或请求。"] : []),
    ],
  };
}
export function mergeReport(report: FindingReport): MergedFindingReport;
export function mergeReport(finding: FindingReportInput): MergedFindingReport;
export function mergeReport(input: FindingReport | FindingReportInput): MergedFindingReport {
  const report = "schema" in input ? input : normalizeReport(input);
  const mergedSteps = deriveSteps(report);
  return {
    ...report,
    mergedSteps,
    verify: buildVerify(report),
    metrics: { ...report.metrics, steps: mergedSteps.length },
  };
}

/** Template naming aliases for callers migrating from the standalone pipeline. */
export const normalize = normalizeReport;
export const mergeSteps = deriveSteps;
