import assert from "node:assert/strict";
import test from "node:test";
import { readFile } from "node:fs/promises";
import { setImmediate as tick, setTimeout as delay } from "node:timers/promises";
import { startSerialPoll } from "./serial-poller.ts";
import { startActivityStream } from "./activity-stream.ts";
import { parseExecutionAttempts } from "./execution-contract.ts";

test("refresh waits for the old request and discards its stale result", async () => {
  const requests = [];
  const values = [];
  const poll = startSerialPoll({
    fetch: (signal) => new Promise((resolve) => requests.push({ signal, resolve })),
    receive: (value) => values.push(value),
    error: () => assert.fail("unexpected error"),
    interval: 60000,
  });
  try {
    poll.refresh();
    poll.refresh();
    assert.equal(requests.length, 1);
    assert.equal(requests[0].signal.aborted, true);
    requests[0].resolve("stale");
    await tick();
    assert.equal(requests.length, 2);
    assert.deepEqual(values, []);
    requests[1].resolve("new");
    await tick();
    assert.deepEqual(values, ["new"]);
  } finally {
    poll.stop();
  }
});

test("unmounted poll suppresses a transport response that ignores cancellation", async () => {
  let resolve;
  let received = false;
  const poll = startSerialPoll({
    fetch: () =>
      new Promise((r) => {
        resolve = r;
      }),
    receive: () => {
      received = true;
    },
    error: () => {},
    interval: 1,
  });
  poll.stop();
  resolve("late");
  await tick();
  assert.equal(received, false);
});

test("SSE closes a failed ticket and reconnects after the last accepted frame", async () => {
  const cursors = [],
    sources = [],
    values = [];
  const stream = startActivityStream({
    cursor: 5,
    retryMs: 1,
    url: async (cursor) => {
      cursors.push(cursor);
      return `ticket-${cursors.length}`;
    },
    connect: () => {
      const source = {
        close() {
          this.closed = true;
        },
      };
      sources.push(source);
      return source;
    },
    parse: (item) => item,
    receive: (item) => values.push(item.seq),
    live: () => {},
  });
  try {
    await tick();
    sources[0].onmessage({ data: '{"seq":6}' });
    sources[0].onmessage({ data: '{"seq":6}' });
    sources[0].onerror();
    sources[0].onerror();
    assert.equal(sources[0].closed, true);
    await delay(15);
    assert.deepEqual(cursors, [5, 6]);
    sources[0].onmessage({ data: '{"seq":99}' });
    sources[1].onmessage({ data: '{"seq":7}' });
    assert.deepEqual(values, [6, 7]);
  } finally {
    stream.stop();
  }
});

test("stopping while a ticket is loading never opens a connection", async () => {
  let resolve;
  const stream = startActivityStream({
    cursor: 0,
    url: () =>
      new Promise((r) => {
        resolve = r;
      }),
    connect: () => assert.fail("connection opened after cleanup"),
    parse: (v) => v,
    receive: () => {},
    live: () => {},
  });
  stream.stop();
  resolve("late-ticket");
  await tick();
});

test("browser parser accepts actual Go JSON and rejects contract drift", async () => {
  const fixture = JSON.parse(await readFile(new URL("./api-contracts.fixture.json", import.meta.url), "utf8"));
  assert.equal(parseExecutionAttempts(fixture)[0].state, "unknown");
  assert.throws(() => parseExecutionAttempts({ items: [{ ...fixture.items[0], task_id: "2" }] }));
  assert.throws(() => parseExecutionAttempts({ attempts: fixture.items }));
  assert.throws(() => parseExecutionAttempts({ items: [{ ...fixture.items[0], state: "maybe" }] }));
});
