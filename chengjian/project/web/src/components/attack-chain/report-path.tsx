"use client";

import type { LucideIcon } from "lucide-react";
import {
  ArrowRight,
  Database,
  FileCode2,
  FlaskConical,
  KeyRound,
  Network,
  ShieldOff,
  Terminal,
  Upload,
  UserRound,
} from "lucide-react";

import type { MergedFindingReport, ReportSource, ReportStep } from "@/lib/finding-report/types";

import styles from "./template-report.module.css";

export interface ReportPathNode {
  id: string;
  title: string;
  detail: string;
  icon: LucideIcon;
  step?: ReportStep;
  source?: ReportSource;
  impact?: boolean;
}

function behaviorIcon(step: ReportStep): LucideIcon {
  if (/消融|对照/.test(step.label)) return FlaskConical;
  if (/写入|上传|修改|删除/.test(step.label)) return Upload;
  if (/读取|数据回显|真实数据/.test(step.label)) return Database;
  if (/脚本|前端|源码|\bJS\b/i.test(step.label)) return FileCode2;
  if (/签名|密钥|凭据|token|secret/i.test(step.label)) return KeyRound;
  if (/鉴权|认证|绕过/.test(step.label)) return ShieldOff;
  return step.request.length ? Terminal : Network;
}

/** Labels describe stored material. No condition, result or dependency is invented. */
export function reportPath(report: MergedFindingReport): { inputs: ReportPathNode[]; actions: ReportPathNode[] } {
  const steps = report.mergedSteps;
  const source = report.sections.find(
    (section) =>
      section.key === "cause" &&
      /前端|JavaScript|\bJS\b/i.test(section.text) &&
      /公开|硬编码/.test(section.text) &&
      /凭据|secret|密钥/i.test(section.text),
  );
  const identity = steps.find((step) => /工号|员工/.test(step.label));
  const comparison = steps.find((step) => /消融|对照/.test(step.label));
  const matrix = steps.find((step) => /消融|对照/.test(step.label) && step.output.length > 0);
  const impact = steps.find((step) => /真实数据.*回显|数据回显|响应.*数据/.test(step.label));
  if (source && identity && comparison && matrix && impact) {
    return {
      inputs: [
        {
          id: "source",
          title: "公开凭据来源",
          detail: "前端产物 · 报告根因记录",
          icon: FileCode2,
          source: source.source,
        },
        { id: "identity", title: "员工身份条件", detail: "核对工号与凭据的触发条件", icon: UserRound, step: identity },
      ],
      actions: [
        { id: "gateway", title: "登录校验对照", detail: "逐头增删 · 查看原始结果", icon: ShieldOff, step: matrix },
        {
          id: "authorization",
          title: "身份组合验证",
          detail: "查看组合条件与返回结果",
          icon: KeyRound,
          step: comparison,
        },
        {
          id: "impact",
          title: "真实数据回显",
          detail: "报告摘录 · 保留证据边界",
          icon: Database,
          step: impact,
          impact: true,
        },
      ],
    };
  }
  const records = steps.filter((step) => step.origin !== "evidence");
  const candidates = records.length ? records : steps;
  return {
    inputs: [],
    actions: candidates.slice(0, 5).map((step) => ({
      id: step.id,
      title: step.label.replace(/^[\d\s.)、]+/, "").replace(/[*`]/g, ""),
      detail: `${step.request.length} 请求 · ${step.response.length} 响应 · ${step.output.length} 输出`,
      icon: behaviorIcon(step),
      step,
    })),
  };
}

export function ReportPath({
  model,
  activeId,
  playingIndex,
  onStep,
  onSource,
}: {
  model: ReturnType<typeof reportPath>;
  activeId?: string;
  playingIndex: number;
  onStep: (step: ReportStep) => void;
  onSource?: (source: ReportSource) => void;
}) {
  const nodes = [...model.inputs, ...model.actions];
  function renderNode(node: ReportPathNode, input = false) {
    const Icon = node.icon;
    return (
      <button
        key={node.id}
        type="button"
        className={styles.behaviorNode}
        data-input={input}
        data-impact={!!node.impact}
        data-path-node={node.id}
        data-path-step={node.step?.id}
        aria-pressed={!!node.step && node.step.id === activeId}
        data-playing={nodes.indexOf(node) === playingIndex}
        onClick={() => {
          if (node.step) onStep(node.step);
          else if (node.source) onSource?.(node.source);
        }}
      >
        <span className={styles.behaviorIcon}>
          <Icon size={25} />
        </span>
        <span>
          <strong>{node.title}</strong>
          <small>{node.detail}</small>
        </span>
      </button>
    );
  }
  return (
    <div className={styles.behaviorPath} data-branched={model.inputs.length > 0}>
      {model.inputs.length > 0 && (
        <>
          <div className={styles.behaviorInputs}>{model.inputs.map((node) => renderNode(node, true))}</div>
          <div className={styles.behaviorJoin} aria-hidden="true">
            <svg viewBox="0 0 42 140" aria-hidden="true">
              <path d="M0 25 H10 Q20 25 20 38 V102 Q20 115 30 115 H42 M0 115 H42" />
            </svg>
            <span>组合</span>
          </div>
        </>
      )}
      <div className={styles.behaviorActions}>
        {model.actions.map((node, index) => (
          <div key={node.id}>
            {renderNode(node)}
            {index < model.actions.length - 1 && <ArrowRight size={18} className={styles.behaviorArrow} />}
          </div>
        ))}
      </div>
    </div>
  );
}
