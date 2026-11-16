"use client";

import * as React from "react";

import { getLocalStorageValue, setLocalStorageValue } from "@/lib/local-storage.client";

const STORAGE_KEY = "chengjian_finding_column_widths_v1";
const DEFAULT_PIXELS = [32, 32, 80, 500, 176, 112, 144, 120, 200];
const MIN_PIXELS = [32, 32, 64, 240, 100, 96, 96, 112, 200];
const DEFAULT_WIDTHS = DEFAULT_PIXELS.map(
  (width) => (width / DEFAULT_PIXELS.reduce((sum, value) => sum + value, 0)) * 100,
);

export function useFindingColumns() {
  const [widths, setWidths] = React.useState(DEFAULT_WIDTHS);
  const [hydrated, setHydrated] = React.useState(false);
  React.useEffect(() => {
    try {
      const saved: unknown = JSON.parse(getLocalStorageValue(STORAGE_KEY) ?? "null");
      if (
        Array.isArray(saved) &&
        saved.length === DEFAULT_WIDTHS.length &&
        saved.every((width) => typeof width === "number" && Number.isFinite(width) && width > 0) &&
        Math.abs(saved.reduce((sum, width) => sum + width, 0) - 100) < 0.1
      ) {
        setWidths(saved);
      }
    } catch {
      // Use the defaults when stored preferences are invalid.
    }
    setHydrated(true);
  }, []);
  React.useEffect(() => {
    if (hydrated) setLocalStorageValue(STORAGE_KEY, JSON.stringify(widths));
  }, [hydrated, widths]);
  return { widths, setWidths };
}

export function FindingColumnResize({
  index,
  label,
  onChange,
}: {
  index: number;
  label: string;
  onChange: (widths: number[]) => void;
}) {
  const drag = React.useRef<{ x: number; tableWidth: number; widths: number[] } | null>(null);

  function measure(target: HTMLElement) {
    const table = target.closest("table");
    if (!table) return null;
    const tableWidth = table.getBoundingClientRect().width;
    const widths = Array.from(table.querySelectorAll("thead th")).map(
      (head) => (head.getBoundingClientRect().width / tableWidth) * 100,
    );
    return { tableWidth, widths };
  }

  function resize(start: { tableWidth: number; widths: number[] }, pixels: number) {
    const { tableWidth, widths } = start;
    const min = (MIN_PIXELS[index] / tableWidth) * 100 - widths[index];
    const max = widths[index + 1] - (MIN_PIXELS[index + 1] / tableWidth) * 100;
    if (min > max) return;
    const delta = Math.max(min, Math.min(max, (pixels / tableWidth) * 100));
    const next = [...widths];
    next[index] += delta;
    next[index + 1] -= delta;
    onChange(next);
  }

  return (
    <button
      type="button"
      aria-label={`调整${label}列宽`}
      title="左右拖动调整列宽；方向键微调；双击恢复默认"
      className="absolute inset-y-0 right-0 z-10 w-2 translate-x-1/2 cursor-col-resize touch-none select-none after:absolute after:inset-y-2 after:left-1/2 after:w-px after:bg-border hover:after:w-0.5 hover:after:bg-primary focus-visible:outline-2 focus-visible:outline-ring"
      onClick={(event) => event.stopPropagation()}
      onDoubleClick={() => onChange(DEFAULT_WIDTHS)}
      onPointerDown={(event) => {
        if (event.button !== 0) return;
        const start = measure(event.currentTarget);
        if (!start) return;
        event.preventDefault();
        event.stopPropagation();
        drag.current = { ...start, x: event.clientX };
        event.currentTarget.setPointerCapture(event.pointerId);
      }}
      onPointerMove={(event) => {
        if (drag.current) resize(drag.current, event.clientX - drag.current.x);
      }}
      onPointerUp={(event) => {
        drag.current = null;
        if (event.currentTarget.hasPointerCapture(event.pointerId)) {
          event.currentTarget.releasePointerCapture(event.pointerId);
        }
      }}
      onLostPointerCapture={() => {
        drag.current = null;
      }}
      onPointerCancel={() => {
        drag.current = null;
      }}
      onKeyDown={(event) => {
        if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
        event.preventDefault();
        const start = measure(event.currentTarget);
        if (start) resize(start, event.key === "ArrowRight" ? 16 : -16);
      }}
    />
  );
}
