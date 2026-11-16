"use client";

import { useState } from "react";

import { ArrowRight } from "lucide-react";

import type { ReportBlock } from "@/lib/finding-report/types";

import styles from "./template-report.module.css";

export function comparisonRows(block: ReportBlock) {
  // Only recognize explicit four-column captures: condition | HTTP | bytes | result.
  const rows = block.body
    .split("\n")
    .filter((line) => line.trim())
    .map((line) => {
      const parts = line.split("|").map((part) => part.trim());
      if (parts.length !== 4 || !/^\d{3}$/.test(parts[1]) || !/^\d+$/.test(parts[2])) return null;
      return { condition: parts[0], http: parts[1], bytes: parts[2], result: parts[3] };
    });
  return rows.length >= 3 && rows.every((row) => row !== null) ? rows.filter((row) => row !== null) : [];
}

export function ReportComparison({ block }: { block: ReportBlock }) {
  const rows = comparisonRows(block);
  const [index, setIndex] = useState(0);
  const row = rows[Math.min(index, rows.length - 1)];
  if (!row) return null;
  return (
    <div className={styles.comparison}>
      <div className={styles.conditionList}>
        {rows.map((item, i) => (
          <button key={item.condition} type="button" aria-pressed={i === index} onClick={() => setIndex(i)}>
            <span>{item.condition}</span>
            {i === index && <ArrowRight size={15} />}
          </button>
        ))}
      </div>
      <div className={styles.conditionResult} aria-live="polite">
        <span>报告记录的返回结果</span>
        <strong>{row.result.split(/\s+<-/)[0]}</strong>
        <p>{row.condition}</p>
        <dl>
          <div>
            <dt>HTTP 状态</dt>
            <dd>{row.http}</dd>
          </div>
          <div>
            <dt>响应大小</dt>
            <dd>{Number(row.bytes).toLocaleString()} B</dd>
          </div>
        </dl>
        {row.result.includes("<-") && <p>{row.result.split("<-").slice(1).join("<-").trim()}</p>}
        <small>业务返回值与 HTTP 状态分别展示；未重新执行请求。</small>
      </div>
    </div>
  );
}
