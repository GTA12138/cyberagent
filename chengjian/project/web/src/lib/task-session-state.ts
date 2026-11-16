import type { Activity } from "./types";

// One session's lazily-loaded, reverse-paginated cache. `lastTs`/`unread` are kept
// live even for sessions that were never opened, so the list shows liveness + unread
// without holding their full history.
export type SessionState = {
  items: Activity[];
  loaded: boolean;
  loading: boolean;
  loadingMore: boolean;
  hasMore: boolean; // older history remains above the loaded window
  earliestSeq: number; // earliest loaded id — reverse-pagination anchor
  unread: number;
  lastTs: string; // most-recent activity time (drives live badge; updated even when unloaded)
  error?: string;
};
export type SessionStore = Record<string, SessionState>;

export function emptyState(): SessionState {
  return {
    items: [],
    loaded: false,
    loading: false,
    loadingMore: false,
    hasMore: false,
    earliestSeq: 0,
    unread: 0,
    lastTs: "",
  };
}

// sessionKeyOf routes an activity to its stable session key. worker="planner" covers
// BOTH the Goal Agent's round-0 decomposition and the Planner (single Plan session).
export function sessionKeyOf(a: Activity): string {
  if (a.worker === "system" || a.kind === "llm_switch" || a.kind === "llm_failover") return "system";
  if (a.worker === "mainagent") return `main:${a.main_seg ?? 0}`; // one key per conversation segment
  if (a.worker === "planner") return "plan";
  if (a.intent_id) return `intent:${a.intent_id}`;
  return "unknown";
}

// mergeBySeq unions two activity lists by seq (dedup) in ascending seq order. Every
// data source — latest page, older page, SSE compensation, live tail — goes through
// this, so history responses can never clobber live records received meanwhile.
export function mergeBySeq(current: Activity[], incoming: Activity[]): Activity[] {
  if (!incoming.length) return current;
  const bySeq = new Map<number, Activity>();
  for (const a of current) bySeq.set(a.seq, a);
  for (const a of incoming) bySeq.set(a.seq, a);
  return [...bySeq.values()].sort((p, q) => p.seq - q.seq);
}

// Cold sessions retain only a small usage tail. Loaded sessions remain bounded,
// except while the user is reading older history. Replay must not add unread twice.
export function mergeSessionActivity(
  previous: SessionStore,
  activity: Activity,
  activeKey: string,
  atBottom: boolean,
  maxKeep = 4000,
): SessionStore {
  const key = sessionKeyOf(activity);
  const current = previous[key] ?? emptyState();
  if (current.items.some((item) => item.seq === activity.seq)) return previous;
  if (!current.loaded && !current.loading && key !== activeKey) {
    const items =
      activity.kind === "usage" || activity.kind === "result"
        ? mergeBySeq(current.items, [activity]).slice(-4)
        : current.items;
    return { ...previous, [key]: { ...current, items, lastTs: activity.ts, unread: current.unread + 1 } };
  }
  let items = mergeBySeq(current.items, [activity]);
  let hasMore = current.hasMore,
    earliestSeq = current.earliestSeq;
  if ((key !== activeKey || atBottom) && items.length > maxKeep) {
    items = items.slice(-maxKeep);
    hasMore = true;
    earliestSeq = items[0].seq;
  }
  return {
    ...previous,
    [key]: {
      ...current,
      items,
      lastTs: activity.ts,
      unread: key === activeKey ? 0 : current.unread + 1,
      hasMore,
      earliestSeq,
    },
  };
}
