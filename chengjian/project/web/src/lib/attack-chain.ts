export interface AttackChainSource {
  id: string;
  kind: "summary" | "evidence" | "report" | "file" | "step" | "node" | "activity";
  label: string;
  locator: string;
  text: string;
  line?: number;
}

export interface AttackChainNode {
  id: string;
  kind: string;
  title: string;
  summary: string;
  basis: "reported" | "recorded" | "missing";
  sources: string[];
  finding_id?: string;
  reference?: string;
  items: string[];
  action?: string;
  endpoint?: string;
  ordinal?: number;
}

export interface AttackChainEdge {
  id: string;
  source: string;
  target: string;
  kind: "narrative" | "reference" | "recorded";
  label: string;
}

export interface AttackChain {
  finding_id: string;
  title: string;
  revision: string;
  nodes: AttackChainNode[];
  edges: AttackChainEdge[];
  sources: AttackChainSource[];
  warnings: string[];
  traffic_count: number;
}
