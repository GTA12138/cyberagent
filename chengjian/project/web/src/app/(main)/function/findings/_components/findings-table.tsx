"use client";

import Link from "next/link";
import { useRouter } from "next/navigation";

import {
  ArrowDownIcon,
  ArrowUpDownIcon,
  ArrowUpIcon,
  ArrowUpRightIcon,
  ChevronRightIcon,
  FlaskConicalIcon,
  RotateCcwIcon,
  Trash2Icon,
} from "lucide-react";

import { StatusBadge } from "@/components/status-badge";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select";
import { Spinner } from "@/components/ui/spinner";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { statusMeta } from "@/lib/status";
import type { ActiveFindingRetest, Finding, FindingQuery, FindingStatus, Severity } from "@/lib/types";

import { FindingColumnResize } from "./finding-columns";

export const SEVERITIES: Severity[] = ["critical", "high", "medium", "low"];

export const FINDING_STATUSES: FindingStatus[] = [
  "pending",
  "in_progress",
  "confirmed",
  "resolved",
  "fixed",
  "false_positive",
  "ignored",
  "duplicate",
  "risk_accepted",
];

export const UNASSIGNED_TASK = "__unassigned__";

// Exploration node ids are only unique inside a task. Prefer the persisted
// finding id and otherwise namespace the node id by task so editing one group
// cannot update a similarly-named node in another expanded group.
export function findingRowKey(finding: Finding): string {
  if (finding.finding_id) return `finding:${finding.finding_id}`;
  return `node:${finding.task_id ?? UNASSIGNED_TASK}:${finding.id}`;
}

export function isSameFinding(left: Finding, right: Finding): boolean {
  return findingRowKey(left) === findingRowKey(right);
}

export function fmtTime(ts: string) {
  return new Date(ts).toLocaleString("zh-CN", {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  });
}

const COLUMN_LABELS = ["选择", "详情", "严重度", "漏洞名称", "资产", "状态", "所属任务", "时间", "操作"];
const COLUMN_COUNT = COLUMN_LABELS.length;

interface FindingsTableProps {
  items: Finding[];
  selectedIds: Set<string>;
  onToggleSelected: (id: string, checked: boolean) => void;
  onToggleSelectedPage: (ids: string[], checked: boolean) => void;
  columnWidths: number[];
  onColumnWidthsChange: (widths: number[]) => void;
  sort: NonNullable<FindingQuery["sort"]>;
  onSortChange: (sort: NonNullable<FindingQuery["sort"]>) => void;
  onStatusChange: (finding: Finding, next: FindingStatus) => void;
  onRetest: (finding: Finding) => void;
  activeRetests: Record<string, ActiveFindingRetest>;
  onDeepen: (finding: Finding) => void;
  onDelete: (finding: Finding) => void;
  /** 全选框的无障碍标签,平铺视图与分组视图措辞不同。 */
  selectAllLabel?: string;
}

// Shared list rows for flat, task, and asset views.
export function FindingsTable({
  items,
  selectedIds,
  onToggleSelected,
  onToggleSelectedPage,
  columnWidths,
  onColumnWidthsChange,
  sort,
  onSortChange,
  onStatusChange,
  onRetest,
  activeRetests,
  onDeepen,
  onDelete,
  selectAllLabel = "选择当前页全部",
}: FindingsTableProps) {
  const router = useRouter();
  const selectableIds = items.map((finding) => finding.finding_id).filter((id): id is string => Boolean(id));
  const selectedCount = selectableIds.filter((id) => selectedIds.has(id)).length;
  let headerChecked: boolean | "indeterminate" = false;
  if (selectableIds.length > 0 && selectedCount === selectableIds.length) {
    headerChecked = true;
  } else if (selectedCount > 0) {
    headerChecked = "indeterminate";
  }

  return (
    <Table className="min-w-[87.25rem] table-fixed" aria-label="漏洞列表">
      <colgroup>
        {columnWidths.map((width, index) => (
          <col key={COLUMN_LABELS[index]} style={{ width: `${width}%` }} />
        ))}
      </colgroup>
      <TableHeader>
        <TableRow>
          <TableHead scope="col">
            <Checkbox
              checked={headerChecked}
              onCheckedChange={(checked) => onToggleSelectedPage(selectableIds, checked === true)}
              aria-label={selectAllLabel}
            />
          </TableHead>
          <TableHead scope="col">
            <span className="sr-only">查看详情</span>
          </TableHead>
          {["严重度", "漏洞名称", "资产", "状态", "所属任务", "时间", "操作"].map((label, i) => {
            const index = i + 2;
            const isTime = label === "时间";
            let SortIcon = ArrowUpDownIcon;
            let timeOrder: "ascending" | "descending" | "none" = "none";
            if (sort === "time") {
              SortIcon = ArrowDownIcon;
              timeOrder = "descending";
            }
            if (sort === "time_asc") {
              SortIcon = ArrowUpIcon;
              timeOrder = "ascending";
            }
            return (
              <TableHead key={label} scope="col" className="relative" aria-sort={isTime ? timeOrder : undefined}>
                {isTime ? (
                  <button
                    type="button"
                    className="inline-flex items-center gap-1 rounded-sm py-1 hover:text-primary focus-visible:outline-2 focus-visible:outline-ring"
                    onClick={() => onSortChange(sort === "time" ? "time_asc" : "time")}
                    title={sort === "time" ? "切换为最早优先" : "切换为最新优先"}
                  >
                    时间 <SortIcon className="size-3.5" aria-hidden="true" />
                  </button>
                ) : (
                  label
                )}
                {index < COLUMN_COUNT - 1 && (
                  <FindingColumnResize index={index} label={label} onChange={onColumnWidthsChange} />
                )}
              </TableHead>
            );
          })}
        </TableRow>
      </TableHeader>
      <TableBody>
        {items.map((f) => {
          const rowKey = findingRowKey(f);
          const retest = f.finding_id ? activeRetests[f.finding_id] : undefined;
          return (
            <TableRow
              key={rowKey}
              className={f.finding_id ? "cursor-pointer" : undefined}
              role={f.finding_id ? "link" : undefined}
              tabIndex={f.finding_id ? 0 : undefined}
              onClick={() => {
                if (f.finding_id) router.push(`/function/findings/detail?id=${encodeURIComponent(f.finding_id)}`);
              }}
              onKeyDown={(event) => {
                if (event.target !== event.currentTarget || (event.key !== "Enter" && event.key !== " ")) return;
                event.preventDefault();
                if (f.finding_id) router.push(`/function/findings/detail?id=${encodeURIComponent(f.finding_id)}`);
              }}
            >
              <TableCell onClick={(e) => e.stopPropagation()}>
                {f.finding_id && (
                  <Checkbox
                    checked={selectedIds.has(f.finding_id)}
                    onCheckedChange={(c) => onToggleSelected(f.finding_id as string, c === true)}
                    aria-label="选择该漏洞"
                  />
                )}
              </TableCell>
              <TableCell>
                {f.finding_id && <ChevronRightIcon className="size-4 text-muted-foreground" aria-hidden="true" />}
              </TableCell>
              <TableCell>
                <StatusBadge domain="severity" value={f.severity} dot />
              </TableCell>
              <TableCell>
                <div className="flex min-w-0 flex-col gap-0.5">
                  {f.finding_id ? (
                    <Link
                      href={`/function/findings/detail?id=${encodeURIComponent(f.finding_id)}`}
                      onClick={(e) => e.stopPropagation()}
                      className="truncate font-medium hover:text-primary hover:underline"
                      title="查看发现详情"
                    >
                      {f.name || f.vulnclass || "未分类"}
                    </Link>
                  ) : (
                    <span className="truncate font-medium">{f.name || f.vulnclass || "未分类"}</span>
                  )}
                  <span className="truncate text-xs text-muted-foreground">{f.summary}</span>
                  <Badge variant="outline">流量证据 {f.traffic_count ?? 0} 条</Badge>
                </div>
              </TableCell>
              <TableCell>
                {f.assets && f.assets.length > 0 ? (
                  <div className="flex flex-wrap gap-1">
                    {f.assets.slice(0, 3).map((a) => (
                      <code
                        key={a.id}
                        className="max-w-full truncate rounded bg-muted px-1.5 py-0.5 font-mono text-xs"
                        title={`${a.type} · ${a.label}`}
                      >
                        {a.label}
                      </code>
                    ))}
                    {f.assets.length > 3 && (
                      <span className="text-xs text-muted-foreground">+{f.assets.length - 3}</span>
                    )}
                  </div>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell onClick={(e) => e.stopPropagation()}>
                {f.finding_id ? (
                  <Select value={f.status} onValueChange={(v) => onStatusChange(f, v as FindingStatus)}>
                    <SelectTrigger size="sm" className="h-7 w-full border-none px-1 shadow-none focus-visible:ring-0">
                      <StatusBadge domain="finding" value={f.status} dot />
                    </SelectTrigger>
                    <SelectContent position="popper" align="end">
                      {FINDING_STATUSES.map((st) => (
                        <SelectItem key={st} value={st}>
                          {statusMeta("finding", st).label}
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                ) : (
                  <StatusBadge domain="finding" value={f.status} dot />
                )}
              </TableCell>
              <TableCell>
                {f.task_id ? (
                  <Link
                    href={`/function/tasks/detail?id=${f.task_id}`}
                    onClick={(e) => e.stopPropagation()}
                    className="inline-flex max-w-full items-center gap-1 text-primary hover:underline"
                    title={f.task_description}
                  >
                    <span className="truncate">{f.task_description}</span>
                    <ArrowUpRightIcon className="size-3 shrink-0" />
                  </Link>
                ) : (
                  <span className="text-muted-foreground">—</span>
                )}
              </TableCell>
              <TableCell className="text-xs text-muted-foreground tabular-nums">{fmtTime(f.ts)}</TableCell>
              <TableCell onClick={(e) => e.stopPropagation()}>
                <div className="flex items-center gap-1">
                  {retest ? (
                    <Button asChild size="sm" variant="ghost">
                      <Link href={`/chat?c=${retest.conversation_id}`} title="查看正在进行的复测会话">
                        <Spinner data-icon="inline-start" />
                        复测中
                      </Link>
                    </Button>
                  ) : null}
                  {!retest && f.finding_id && !f.inherited ? (
                    <Button size="sm" variant="ghost" onClick={() => onRetest(f)} title="在独立会话中复测该漏洞">
                      <RotateCcwIcon data-icon="inline-start" />
                      复测
                    </Button>
                  ) : null}
                  {f.finding_id && f.task_id && (
                    <Button size="sm" variant="ghost" onClick={() => onDeepen(f)}>
                      <FlaskConicalIcon data-icon="inline-start" />
                      深入
                    </Button>
                  )}
                  {f.finding_id && (
                    <AlertDialog>
                      <AlertDialogTrigger asChild>
                        <Button
                          size="icon"
                          variant="ghost"
                          className="size-7 text-muted-foreground hover:text-destructive"
                          aria-label="删除漏洞"
                        >
                          <Trash2Icon className="size-4" />
                        </Button>
                      </AlertDialogTrigger>
                      <AlertDialogContent>
                        <AlertDialogHeader>
                          <AlertDialogTitle>确认删除该漏洞？</AlertDialogTitle>
                          <AlertDialogDescription className="break-words">
                            「
                            <span className="break-all">
                              {f.name || f.vulnclass || f.summary || `#${f.finding_id}`}
                            </span>
                            」将被永久删除， 同时从发现列表、任务发现 Tab 与探索图中移除，此操作不可撤销。
                          </AlertDialogDescription>
                        </AlertDialogHeader>
                        <AlertDialogFooter>
                          <AlertDialogCancel>取消</AlertDialogCancel>
                          <AlertDialogAction onClick={() => onDelete(f)}>删除</AlertDialogAction>
                        </AlertDialogFooter>
                      </AlertDialogContent>
                    </AlertDialog>
                  )}
                </div>
              </TableCell>
            </TableRow>
          );
        })}
        {items.length === 0 && (
          <TableRow>
            <TableCell colSpan={COLUMN_COUNT} className="py-12 text-center text-sm text-muted-foreground">
              没有匹配的发现。
            </TableCell>
          </TableRow>
        )}
      </TableBody>
    </Table>
  );
}
