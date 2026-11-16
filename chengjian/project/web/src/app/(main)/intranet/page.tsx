"use client";

import * as React from "react";

import { Loader2Icon, NetworkIcon, RefreshCwIcon } from "lucide-react";

import { ModuleOverview } from "@/components/console/module-overview";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { api } from "@/lib/api";
import type { Credential, IntranetTopology, ShellSession, Task, TunnelInfo } from "@/lib/types";
import { cn } from "@/lib/utils";

import { CredsTab } from "./_components/creds-tab";
import { SessionWorkbench } from "./_components/session-workbench";
import { type HostFilter, SessionsTab } from "./_components/sessions-tab";
import { TopologyGraph } from "./_components/topology-graph";
import { TunnelsTab } from "./_components/tunnels-tab";

const SESSIONS_POLL_MS = 10_000;

export default function IntranetPage() {
  const [topology, setTopology] = React.useState<IntranetTopology | null>(null);
  const [sessions, setSessions] = React.useState<ShellSession[]>([]);
  const [tunnels, setTunnels] = React.useState<TunnelInfo[]>([]);
  const [credentials, setCredentials] = React.useState<Credential[]>([]);
  const [loadingTopology, setLoadingTopology] = React.useState(true);
  const [loadingSessions, setLoadingSessions] = React.useState(true);
  const [loadingTunnels, setLoadingTunnels] = React.useState(true);
  const [loadingCreds, setLoadingCreds] = React.useState(true);

  const [tab, setTab] = React.useState("sessions");
  const [hostFilter, setHostFilter] = React.useState<HostFilter | null>(null);
  const [workbenchSession, setWorkbenchSession] = React.useState<ShellSession | null>(null);

  // 任务过滤:"all" = 全局视图(后端不传 task_id);四个台账 + 拓扑共用同一选择。
  const [taskId, setTaskId] = React.useState("all");
  const [tasks, setTasks] = React.useState<Task[]>([]);
  const taskParam = taskId === "all" ? undefined : taskId;
  const currentScope = React.useRef(taskId);
  currentScope.current = taskId;
  const requests = React.useRef({ topology: 0, sessions: 0, tunnels: 0, credentials: 0 });
  const [errors, setErrors] = React.useState<Record<string, string>>({});

  React.useEffect(() => {
    api
      .tasks()
      .then((r) => setTasks(r.tasks))
      .catch(() => {
        /* 任务列表拉取失败不阻塞台账 */
      });
  }, []);

  const loadTopology = React.useCallback(() => {
    const request = ++requests.current.topology;
    const current = () => currentScope.current === taskId && requests.current.topology === request;
    setLoadingTopology(true);
    api
      .intranetTopology(taskParam)
      .then((result) => {
        if (!current()) return;
        setTopology(result);
        setErrors((prev) => ({ ...prev, topology: "" }));
      })
      .catch((error) => { if (current()) setErrors((prev) => ({ ...prev, topology: `拓扑：${(error as Error).message}` })); })
      .finally(() => { if (current()) setLoadingTopology(false); });
  }, [taskParam, taskId]);

  // 会话列表 10s 轮询；签名相同则跳过 setState，避免打开工作台时无谓重渲染。
  const sigRef = React.useRef("");
  const loadSessions = React.useCallback(
    (initial = false) => {
      const request = ++requests.current.sessions;
      const current = () => currentScope.current === taskId && requests.current.sessions === request;
      if (initial) setLoadingSessions(true);
      api
        .shellSessions(taskParam)
        .then((items) => {
          if (!current()) return;
          setErrors((prev) => ({ ...prev, sessions: "" }));
          const sig = JSON.stringify(items);
          if (sig !== sigRef.current) {
            sigRef.current = sig;
            setSessions(items);
          }
        })
        .catch((error) => { if (current()) setErrors((prev) => ({ ...prev, sessions: `会话：${(error as Error).message}` })); })
        .finally(() => { if (current()) setLoadingSessions(false); });
    },
    [taskParam, taskId],
  );

  const loadTunnels = React.useCallback(() => {
    const request = ++requests.current.tunnels;
    const current = () => currentScope.current === taskId && requests.current.tunnels === request;
    setLoadingTunnels(true);
    api
      .tunnels(taskParam)
      .then((result) => {
        if (!current()) return;
        setTunnels(result);
        setErrors((prev) => ({ ...prev, tunnels: "" }));
      })
      .catch((error) => { if (current()) setErrors((prev) => ({ ...prev, tunnels: `隧道：${(error as Error).message}` })); })
      .finally(() => { if (current()) setLoadingTunnels(false); });
  }, [taskParam, taskId]);

  const loadCreds = React.useCallback(() => {
    const request = ++requests.current.credentials;
    const current = () => currentScope.current === taskId && requests.current.credentials === request;
    setLoadingCreds(true);
    api
      .credentials(taskParam)
      .then((result) => {
        if (!current()) return;
        setCredentials(result);
        setErrors((prev) => ({ ...prev, credentials: "" }));
      })
      .catch((error) => { if (current()) setErrors((prev) => ({ ...prev, credentials: `凭据：${(error as Error).message}` })); })
      .finally(() => { if (current()) setLoadingCreds(false); });
  }, [taskParam, taskId]);

  React.useEffect(() => {
    loadTopology();
    loadTunnels();
    loadCreds();
    sigRef.current = ""; // 切换任务后强制刷新会话列表
    loadSessions(true);
    const timer = setInterval(() => loadSessions(), SESSIONS_POLL_MS);
    return () => {
      clearInterval(timer);
      for (const key of Object.keys(requests.current) as (keyof typeof requests.current)[]) ++requests.current[key];
    };
  }, [loadTopology, loadTunnels, loadCreds, loadSessions]);

  const refreshAll = () => {
    loadTopology();
    loadTunnels();
    loadCreds();
    sigRef.current = "";
    loadSessions(true);
  };

  const changeTask = (value: string) => {
    if (value === taskId) return;
    currentScope.current = value;
    setTaskId(value);
    setTopology(null);
    setSessions([]);
    setTunnels([]);
    setCredentials([]);
    setErrors({});
    setHostFilter(null);
    setWorkbenchSession(null);
  };

  // 拓扑节点点击 → 切到「会话」Tab 并按该主机过滤。
  const onSelectHost = React.useCallback(
    (nodeId: string) => {
      const node = topology?.nodes.find((n) => n.id === nodeId);
      setHostFilter({ id: nodeId, ip: node?.ip ?? "" });
      setTab("sessions");
    },
    [topology],
  );

  // 轮询刷新后，工作台下那条会话可能已被删除/状态变化：同步最新记录。
  React.useEffect(() => {
    setWorkbenchSession((cur) => {
      if (!cur) return cur;
      return sessions.find((s) => s.id === cur.id) ?? cur;
    });
  }, [sessions]);

  const aliveSessions = sessions.filter((s) => s.status === "alive").length;

  return (
    <div data-console-module="intranet" className="flex min-h-0 min-w-0 flex-1 flex-col gap-4">
      {/* Header */}
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex items-center gap-2">
          <NetworkIcon className="h-5 w-5 text-muted-foreground" />
          <h1 className="text-xl font-semibold tracking-tight">内网作战</h1>
        </div>
        <div className="flex items-center gap-2">
          {/* 任务过滤:照 findings 页下拉范式;选中值同时作用于拓扑与三台账(含 10s 轮询)。 */}
          <Select value={taskId} onValueChange={changeTask}>
            <SelectTrigger size="sm" className="w-48">
              <SelectValue placeholder="任务" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">全部任务</SelectItem>
              {tasks.map((t) => {
                const id = String(t.id);
                const label = t.name || t.description || `任务 #${id}`;
                return (
                  <SelectItem key={id} value={id}>
                    <span className="max-w-[14rem] truncate" title={label}>
                      {label}
                    </span>
                  </SelectItem>
                );
              })}
            </SelectContent>
          </Select>
          <Button variant="outline" size="sm" className="h-8 gap-1" onClick={refreshAll}>
            <RefreshCwIcon className={cn("size-3.5", loadingTopology && "animate-spin")} />
            刷新
          </Button>
        </div>
      </div>

      <ModuleOverview
        label="内网台账"
        loading={loadingSessions && loadingTunnels && loadingCreds}
        metrics={[
          { label: "存活会话 / 全部会话", value: loadingSessions || errors.sessions ? "—" : `${aliveSessions} / ${sessions.length}` },
          { label: "隧道记录", value: loadingTunnels || errors.tunnels ? "—" : tunnels.length },
          { label: "凭据记录", value: loadingCreds || errors.credentials ? "—" : credentials.length },
          { label: "拓扑节点", value: loadingTopology || errors.topology ? "—" : topology?.nodes.length ?? 0 },
        ]}
      />
      {Object.values(errors).some(Boolean) && (
        <p role="status" className="text-sm text-destructive">
          读取失败：{Object.values(errors).filter(Boolean).join("；")}。请刷新重试，保留的记录仅供参考。
        </p>
      )}

      {/* 上半部：内网拓扑 */}
      <Card>
        <CardContent className="p-0">
          <div className="h-[42vh] w-full overflow-hidden rounded-xl">
            <TopologyGraph
              data={topology}
              loading={loadingTopology}
              onRefresh={loadTopology}
              onSelectHost={onSelectHost}
            />
          </div>
        </CardContent>
      </Card>

      {/* 下半部：会话 / 隧道 / 凭据 */}
      <Tabs value={tab} onValueChange={setTab} className="flex min-h-0 flex-1 flex-col gap-0">
        <TabsList className="w-fit">
          <TabsTrigger value="sessions">会话</TabsTrigger>
          <TabsTrigger value="tunnels">隧道</TabsTrigger>
          <TabsTrigger value="creds">凭据</TabsTrigger>
        </TabsList>
        <TabsContent value="sessions" className="flex min-h-0 flex-1 flex-col pt-2">
          <SessionsTab
            sessions={sessions}
            loading={loadingSessions}
            hostFilter={hostFilter}
            onClearHostFilter={() => setHostFilter(null)}
            onOpenWorkbench={setWorkbenchSession}
            onChanged={() => {
              sigRef.current = "";
              loadSessions(true);
              loadTopology();
            }}
          />
        </TabsContent>
        <TabsContent value="tunnels" className="flex min-h-0 flex-1 flex-col pt-2">
          <TunnelsTab
            tunnels={tunnels}
            loading={loadingTunnels}
            onRefresh={() => {
              loadTunnels();
              loadTopology();
            }}
          />
        </TabsContent>
        <TabsContent value="creds" className="flex min-h-0 flex-1 flex-col pt-2">
          {loadingCreds && credentials.length === 0 ? (
            <div className="py-12 text-center">
              <Loader2Icon className="mx-auto h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          ) : (
            <CredsTab credentials={credentials} loading={loadingCreds} />
          )}
        </TabsContent>
      </Tabs>

      <SessionWorkbench
        session={workbenchSession}
        onOpenChange={(o) => !o && setWorkbenchSession(null)}
        onDeleted={() => {
          sigRef.current = "";
          loadSessions(true);
          loadTopology();
        }}
      />
    </div>
  );
}
