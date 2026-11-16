import assert from "node:assert/strict";
import test from "node:test";
import { emptyState, mergeSessionActivity } from "./task-session-state.ts";

const activity = (seq, kind = "text") => ({
  seq,
  worker: "worker",
  intent_id: 2,
  kind,
  ts: `2026-10-25T01:00:${seq}Z`,
  summary: "event",
});

test("replayed activity does not duplicate unread or transcript rows", () => {
  const initial = { "intent:2": { ...emptyState(), loaded: true } };
  const first = mergeSessionActivity(initial, activity(1), "main:0", true);
  const replay = mergeSessionActivity(first, activity(1), "main:0", true);
  assert.equal(replay, first);
  assert.equal(replay["intent:2"].unread, 1);
  assert.equal(initial["intent:2"].items.length, 0);
});

test("history stays intact while reading; trimming preserves the paging anchor", () => {
  let store = { "intent:2": { ...emptyState(), loaded: true } };
  for (let seq = 1; seq <= 3; seq++) store = mergeSessionActivity(store, activity(seq), "intent:2", false, 2);
  assert.equal(store["intent:2"].items.length, 3);
  const trimmed = mergeSessionActivity(store, activity(4), "intent:2", true, 2)["intent:2"];
  assert.deepEqual(
    trimmed.items.map((item) => item.seq),
    [3, 4],
  );
  assert.equal(trimmed.earliestSeq, 3);
  assert.equal(trimmed.hasMore, true);
});

test("inactive sessions retain a bounded accounting tail", () => {
  let store = {};
  for (let seq = 1; seq <= 10; seq++) store = mergeSessionActivity(store, activity(seq, "usage"), "main:0", true);
  assert.equal(store["intent:2"].items.length, 4);
  assert.equal(store["intent:2"].loaded, false);
});
