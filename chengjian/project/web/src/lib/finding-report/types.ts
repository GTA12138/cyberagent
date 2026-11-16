/** Source positions refer to the stored document, never to a generated excerpt. */
export interface ReportSource {
  findingId: string;
  document: "report" | "evidence" | "summary";
  line: number;
  endLine: number;
  start: number;
  end: number;
  text: string;
}
export type BlockKind =
  | "http-request"
  | "curl"
  | "http-response"
  | "http-headers"
  | "code"
  | "json"
  | "shell"
  | "output";
export interface ReportBlock {
  lang: string;
  body: string;
  kind: BlockKind;
  index: number;
  source: ReportSource;
  raw: string;
  closed: boolean;
  excerpted: boolean;
  warnings: string[];
}
export interface ReportEndpoint {
  method: string;
  path: string;
  source: "explicit" | "path";
  provenance: ReportSource;
}
export interface ReportReferences {
  findings: number[];
  facts: number[];
  intents: number[];
  namespace: "exploration-node";
}
export interface ReportGroup {
  n: number;
  label: string;
  text: string;
  raw: string;
  preamble?: string;
  source: ReportSource;
  blocks: ReportBlock[];
  refs: ReportReferences;
  endpoints: ReportEndpoint[];
}
export interface ReportSection {
  id: string;
  heading: string;
  key: string;
  level: number;
  text: string;
  raw: string;
  source: ReportSource;
  blocks: ReportBlock[];
}
export interface ReportDocument {
  document: ReportSource["document"];
  text: string;
  sections: ReportSection[];
  blocks: ReportBlock[];
}
export interface FindingReportInput {
  id: string | number;
  name?: string;
  severity?: string;
  status?: string;
  vulnclass?: string;
  report?: string;
  evidence?: string;
  summary?: string;
  report_stale?: boolean;
  evidence_version?: number;
  report_evidence_version?: number;
}
export interface ReportStep {
  id: string;
  label: string;
  origin: "repro" | "evidence" | "merged";
  request: ReportBlock[];
  response: ReportBlock[];
  output: ReportBlock[];
  other: ReportBlock[];
  notes: string[];
  sig: string[];
  evidenceLabel?: string;
  sources: ReportSource[];
  match: "none" | "exact-endpoint";
  verification: "reported-only";
}
export interface RequestTemplate {
  method: string;
  path: string;
  host: string;
  scheme: string;
  protocol: string;
  headers: [string, string][];
  body: string;
  source: "report-http-block" | "report-curl" | "scope-only" | "none";
  provenance: ReportSource[];
  missing: string[];
  complete: boolean;
  excerpted: boolean;
}
export interface ReportVerify {
  template: RequestTemplate;
  curl: string;
  raw: string;
  curlKind: "original" | "derived" | "unavailable";
  rawKind: "original" | "derived" | "unavailable";
  sources: ReportSource[];
  incomplete: boolean;
  confidence: "low" | "documented";
  confidenceNote: string;
  burpNote: string;
  warnings: string[];
}
export interface FindingReport {
  schema: "chengjian.finding.report/v1";
  source: { findingId: string | number; format: "legacy-md"; chars: number };
  identity: {
    id: string | number;
    title: string;
    severity: string;
    vulnclass: string;
    asset: string;
    intent: string;
    status: string;
    headerLines: { k: string; v: string }[];
    warnings: string[];
  };
  verdict: { statement: string; points: string[] };
  impacts: { lead: string; body: string }[];
  impactProse: string[];
  scope: { bullets: string[]; endpoints: ReportEndpoint[] };
  reproduction: {
    groups: ReportGroup[];
    numbered: string[];
    blocks: ReportBlock[];
    precondition: string;
    preamble: string;
  };
  evidence: { groups: ReportGroup[]; blocks: ReportBlock[] };
  poc: { intro: string; main: ReportBlock | null; extras: ReportBlock[] };
  rootCause: string[];
  remediation: string[];
  limitations: { heading: string | null; items: string[] };
  relations: ReportReferences;
  metrics: { chars: number; codeBlocks: number; endpoints: number; steps: number; hasTable: boolean };
  /** Lossless, source-ordered content including unknown headings and unterminated fences. */
  documents: ReportDocument[];
  sections: ReportSection[];
  unrecognizedSections: ReportSection[];
  rawMarkdown: string;
  raw: { report: string; evidence: string; summary: string };
  verification: { status: "reported-only"; verified: false; reportStale: boolean; note: string };
}
export interface MergedFindingReport extends FindingReport {
  mergedSteps: ReportStep[];
  verify: ReportVerify;
}
