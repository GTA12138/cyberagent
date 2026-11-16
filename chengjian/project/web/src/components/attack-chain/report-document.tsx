"use client";

import { createContext, createElement, type HTMLAttributes, useContext, useMemo } from "react";

import ReactMarkdown, { type Components, type ExtraProps } from "react-markdown";
import remarkGfm from "remark-gfm";

import { CopyButton } from "@/components/copy-button";
import { cn } from "@/lib/utils";

import styles from "./report-document.module.css";

type DocumentContext = { document: string; activeLine?: number };
const SourceContext = createContext<DocumentContext>({ document: "" });
type SourceNode = ExtraProps["node"];

function sourceAttributes(node: SourceNode, context: DocumentContext) {
  const start = node?.position?.start.line;
  const end = node?.position?.end.line;
  return {
    "data-document": context.document,
    "data-source-line": start,
    "data-source-end": end,
    "data-source-active":
      context.activeLine !== undefined && start !== undefined && end !== undefined
        ? context.activeLine >= start && context.activeLine <= end
        : false,
  };
}

type BlockProps = HTMLAttributes<HTMLElement> & {
  node?: SourceNode;
  as: "p" | "h1" | "h2" | "h3" | "h4" | "h5" | "h6" | "li" | "blockquote" | "tr" | "th" | "td";
};

function SourceBlock({ as, node, ...props }: BlockProps) {
  const context = useContext(SourceContext);
  const response =
    (as === "p" || (as === "li" && !hasParagraph(node))) &&
    /^(?:[-–—]\s*)?(?:(?:响应|返回)(?:结果|内容|报文|状态)?\s*[:：]|(?:响应|返回)\s*(?:为\s*)?[1-5]\d{2}\b|(?:→|->|=>)\s*(?:HTTP\/[\d.]+\s+)?[1-5]\d{2}\b|HTTP\/[\d.]+\s+[1-5]\d{2}\b)/.test(
      nodeText(node).trim(),
    );
  return createElement(
    as,
    {
      ...props,
      tabIndex: -1,
      "data-evidence-type": response ? "response" : undefined,
      ...sourceAttributes(node, context),
    },
    response ? (
      <>
        <span className={styles.responseLabel}>RESPONSE · 报告记录</span>
        {props.children}
      </>
    ) : (
      props.children
    ),
  );
}

type TextNode = { value?: string; tagName?: string; children?: readonly TextNode[] };
function hasParagraph(node: TextNode | undefined): boolean {
  return node?.children?.some((child) => child.tagName === "p" || hasParagraph(child)) ?? false;
}
function nodeText(node: TextNode | undefined): string {
  if (!node) return "";
  if (typeof node.value === "string") return node.value;
  return node.children?.map(nodeText).join("") ?? "";
}

function codeDescription(text: string, declared: string) {
  const request =
    /^\s*(?:GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|CONNECT|TRACE)\s+(?:https?:\/\/\S+|\/\S*|\*)\s*(?:HTTP\/[\d.]+)?\s*$/m.test(
      text,
    );
  const response = /^\s*HTTP\/[\d.]+\s+\d{3}(?:\s+[^\n]*)?$/m.test(text);
  let language = declared;
  if (!language && /^#!.*\bpython[\d.]*\b/m.test(text)) language = "python";
  if (!language && /^#!.*\b(?:ba|z|k)?sh\b/m.test(text)) language = "shell";
  if (!language && (request || response)) language = "http";
  const names: Record<string, string> = {
    js: "JavaScript",
    javascript: "JavaScript",
    ts: "TypeScript",
    typescript: "TypeScript",
    py: "Python",
    python: "Python",
    sh: "Shell",
    bash: "Bash",
    shell: "Shell",
    ps1: "PowerShell",
    powershell: "PowerShell",
    json: "JSON",
    http: "HTTP",
    html: "HTML",
    xml: "XML",
    sql: "SQL",
    go: "Go",
    yaml: "YAML",
    yml: "YAML",
    curl: "cURL",
  };
  let kind = "代码 / 原文";
  if (request && response) kind = "HTTP 请求 / 响应";
  else if (request) kind = "HTTP 请求";
  else if (response) kind = "HTTP 响应";
  return {
    language: names[language.toLowerCase()] ?? (language || "TEXT"),
    kind,
  };
}

function CodeBlock({ node, children, className, ...props }: HTMLAttributes<HTMLPreElement> & ExtraProps) {
  const context = useContext(SourceContext);
  const codeNode = node?.children.find((child) => child.type === "element" && child.tagName === "code");
  const text = nodeText(codeNode ?? node);
  const classes = codeNode && "properties" in codeNode ? String(codeNode.properties.className ?? "") : "";
  const declared = /(?:^|[ ,])language-([^\s,]+)/.exec(classes)?.[1] ?? "";
  const description = codeDescription(text, declared);
  return (
    <div className={styles.codeBlock} tabIndex={-1} {...sourceAttributes(node, context)}>
      <div className={styles.codeHeader}>
        <span className={styles.language}>{description.language}</span>
        <span>{description.kind}</span>
        {node?.position && (
          <span className={styles.codeLines}>
            L{node.position.start.line}–{node.position.end.line}
          </span>
        )}
        <CopyButton text={text} label="复制代码" className={styles.copyButton} size="sm" />
      </div>
      <pre {...props} tabIndex={-1} className={cn(styles.codePre, className)} {...sourceAttributes(node, context)}>
        {children}
      </pre>
    </div>
  );
}

function SourceTable({ node, children, ...props }: HTMLAttributes<HTMLTableElement> & ExtraProps) {
  const context = useContext(SourceContext);
  return (
    <div className={styles.tableScroll}>
      <table {...props} tabIndex={-1} {...sourceAttributes(node, context)}>
        {children}
      </table>
    </div>
  );
}

// Reports can contain HTML payloads. Show their literal source without executing
// it or silently dropping it through react-markdown's default raw-HTML filter.
type LiteralNode = { type: string; value?: string; children?: LiteralNode[] };
function preserveLiteralHtml() {
  return (tree: LiteralNode) => {
    function visit(node: LiteralNode) {
      if (node.type === "raw") node.type = "text";
      for (const child of node.children ?? []) visit(child);
    }
    visit(tree);
  };
}

const components: Components = {
  p: ({ node, ...props }) => <SourceBlock as="p" node={node} {...props} />,
  h1: ({ node, ...props }) => <SourceBlock as="h1" node={node} {...props} />,
  h2: ({ node, ...props }) => <SourceBlock as="h2" node={node} {...props} />,
  h3: ({ node, ...props }) => <SourceBlock as="h3" node={node} {...props} />,
  h4: ({ node, ...props }) => <SourceBlock as="h4" node={node} {...props} />,
  h5: ({ node, ...props }) => <SourceBlock as="h5" node={node} {...props} />,
  h6: ({ node, ...props }) => <SourceBlock as="h6" node={node} {...props} />,
  li: ({ node, ...props }) => <SourceBlock as="li" node={node} {...props} />,
  blockquote: ({ node, ...props }) => <SourceBlock as="blockquote" node={node} {...props} />,
  pre: CodeBlock,
  table: SourceTable,
  tr: ({ node, ...props }) => <SourceBlock as="tr" node={node} {...props} />,
  th: ({ node, ...props }) => <SourceBlock as="th" node={node} {...props} />,
  td: ({ node, ...props }) => <SourceBlock as="td" node={node} {...props} />,
  a: ({ node: _node, ...props }) => <a {...props} target="_blank" rel="noopener noreferrer" />,
  // Preserve image descriptions and addresses without automatically contacting report targets.
  img: ({ node: _node, alt, src }) => (
    <span className={styles.imageReference}>
      {alt && <span>{alt} · </span>}
      <a href={typeof src === "string" ? src : undefined} target="_blank" rel="noopener noreferrer">
        {typeof src === "string" ? src : "图片引用"}
      </a>
    </span>
  ),
};

export function ReportDocument({
  text,
  document,
  activeLine,
}: {
  text: string;
  document: string;
  activeLine?: number;
}) {
  const context = useMemo(() => ({ document, activeLine }), [document, activeLine]);
  return (
    <SourceContext.Provider value={context}>
      <div className={styles.document}>
        <ReactMarkdown remarkPlugins={[remarkGfm]} rehypePlugins={[preserveLiteralHtml]} components={components}>
          {text}
        </ReactMarkdown>
      </div>
    </SourceContext.Provider>
  );
}
