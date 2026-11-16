"use client";

import { type ReactNode, useEffect, useState } from "react";

import { usePathname } from "next/navigation";

import { ChevronRight } from "lucide-react";

import { Separator } from "@/components/ui/separator";
import { SidebarTrigger } from "@/components/ui/sidebar";
import { useCurrentUser } from "@/hooks/use-current-user";
import { api } from "@/lib/api";
import { cn } from "@/lib/utils";
import { sidebarItems } from "@/navigation/sidebar/sidebar-items";

import { AccountSwitcher } from "./sidebar/account-switcher";
import { LayoutControls } from "./sidebar/layout-controls";
import { SearchDialog } from "./sidebar/search-dialog";
import { ThemeSwitcher } from "./sidebar/theme-switcher";
import { UpdateBadge } from "./update-badge";

// 任务详情页保持原样：它自带头部/Tabs 与内边距，这里不再叠加全局头部和 padding。
function isFullBleed(pathname: string) {
  const p = (() => {
    try {
      return decodeURIComponent(pathname);
    } catch {
      return pathname;
    }
  })();
  return p.startsWith("/function/tasks/");
}

export function MainContent({ children }: { children: ReactNode }) {
  const currentUser = useCurrentUser();
  const pathname = usePathname();
  const current = sidebarItems
    .flatMap((group) => group.items.map((item) => ({ ...item, group: group.label })))
    .filter(
      (item) => "url" in item && (pathname.replace(/\/+$/, "") === item.url || pathname.startsWith(`${item.url}/`)),
    )
    .sort((a, b) => ("url" in b ? b.url.length : 0) - ("url" in a ? a.url.length : 0))[0];
  const [version, setVersion] = useState("");

  useEffect(() => {
    api
      .health()
      .then((h) => setVersion((h.version ?? "").replace(/^v(?=\d)/, "")))
      .catch(() => setVersion(""));
  }, []);

  if (isFullBleed(pathname)) {
    return (
      <div className="console-fullbleed flex min-h-0 flex-1 flex-col" data-ui>
        {children}
      </div>
    );
  }

  return (
    <>
      <header
        className={cn(
          "console-topbar flex h-12 shrink-0 items-center gap-2 border-b transition-[width,height] ease-linear group-has-data-[collapsible=icon]/sidebar-wrapper:h-12",
          "[html[data-navbar-style=sticky]_&]:sticky [html[data-navbar-style=sticky]_&]:top-0 [html[data-navbar-style=sticky]_&]:z-50 [html[data-navbar-style=sticky]_&]:overflow-hidden [html[data-navbar-style=sticky]_&]:rounded-t-[inherit] [html[data-navbar-style=sticky]_&]:bg-background/50 [html[data-navbar-style=sticky]_&]:backdrop-blur-md",
        )}
      >
        <div className="flex w-full items-center justify-between px-4 lg:px-6">
          <div className="flex items-center gap-1 lg:gap-2">
            <SidebarTrigger className="-ml-1" />
            <Separator
              orientation="vertical"
              className="mx-2 data-[orientation=vertical]:h-4 data-[orientation=vertical]:self-center"
            />
            {current && (
              <div className="console-location">
                <span>{current.group}</span>
                <ChevronRight size={12} />
                <strong>{current.title}</strong>
              </div>
            )}
            <SearchDialog />
          </div>
          <div className="flex items-center gap-2">
            {version && (
              <span data-console-version className="font-medium text-muted-foreground text-xs tabular-nums">
                版本 · {version}
              </span>
            )}
            <UpdateBadge />
            <LayoutControls />
            <ThemeSwitcher />
            <AccountSwitcher users={[currentUser]} />
          </div>
        </div>
      </header>
      <div
        data-ui
        data-console-page={pathname}
        className="console-page min-h-0 min-w-0 flex-1 overflow-x-hidden p-4 has-data-[content-padding=false]:p-0 has-data-[report-layout=wide]:overflow-x-clip md:p-6 md:has-data-[content-padding=false]:p-0"
      >
        {children}
      </div>
    </>
  );
}
