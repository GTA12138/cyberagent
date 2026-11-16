"use client";

import { type ComponentProps, useCallback, useEffect } from "react";

import { PrefetchKind } from "next/dist/client/components/router-reducer/router-reducer-types";
import Link, { useLinkStatus } from "next/link";
import { useRouter } from "next/navigation";

// Warm code and static route payloads only. Business data always comes from live APIs.
const warmed = new Map<string, number>();
const COMMON_MODULES = ["/dashboard", "/function/tasks", "/function/findings", "/function/assets"];

function useModulePrefetch() {
  const router = useRouter();
  return useCallback(
    (href: string) => {
      const connection = (navigator as Navigator & { connection?: { saveData?: boolean; effectiveType?: string } })
        .connection;
      if (connection?.saveData || /(^|-)2g$/.test(connection?.effectiveType ?? "")) return;
      if (!href.startsWith("/") || href.startsWith("//")) return;
      if (Date.now() - (warmed.get(href) ?? 0) < 300_000) return;
      warmed.set(href, Date.now());
      router.prefetch(href, {
        kind: PrefetchKind.FULL,
        onInvalidate: () => {
          warmed.delete(href);
        },
      });
    },
    [router],
  );
}

export function ModuleWarmup() {
  const prefetch = useModulePrefetch();
  useEffect(() => {
    const connection = (navigator as Navigator & { connection?: { saveData?: boolean; effectiveType?: string } })
      .connection;
    if (connection?.saveData || /(^|-)2g$/.test(connection?.effectiveType ?? "")) return;
    const current = window.location.pathname.replace(/\/+$/, "");
    const queue = COMMON_MODULES.filter((href) => href !== current).slice(0, 3);
    let timer: ReturnType<typeof setTimeout>;
    let idle: number | undefined;
    let disposed = false;
    const warmNext = () => {
      if (disposed || document.hidden) return;
      const href = queue.shift();
      if (!href) return;
      prefetch(href);
      if (queue.length) timer = setTimeout(schedule, 350);
    };
    const schedule = () => {
      if ("requestIdleCallback" in window) idle = window.requestIdleCallback(warmNext, { timeout: 2000 });
      else warmNext();
    };
    timer = setTimeout(schedule, 300);
    return () => {
      disposed = true;
      clearTimeout(timer);
      if (idle !== undefined) window.cancelIdleCallback(idle);
    };
  }, [prefetch]);
  return null;
}

function PendingMark() {
  const { pending } = useLinkStatus();
  return <span className="console-navigation-pending" data-navigation-pending={pending} aria-hidden="true" />;
}

export function ModuleLink({ children, onPointerEnter, onFocus, onTouchStart, ...props }: ComponentProps<typeof Link>) {
  const prefetch = useModulePrefetch();
  const warm = () => {
    if (typeof props.href === "string" && !props.target) prefetch(props.href);
  };
  return (
    <Link
      {...props}
      prefetch={false}
      onPointerEnter={(event) => {
        warm();
        onPointerEnter?.(event);
      }}
      onFocus={(event) => {
        warm();
        onFocus?.(event);
      }}
      onTouchStart={(event) => {
        warm();
        onTouchStart?.(event);
      }}
    >
      {children}
      <PendingMark />
    </Link>
  );
}
