# Local Norma v0.4.3 maintenance

This directory was copied from the existing Go module cache, without changing
the preserved ARTEX checkout or the shared module cache. Its upstream module
identity and notices are retained for attribution and import compatibility.

CHENGJIAN changes cover local process resource limits/cancellation/telemetry,
platform-specific shell guidance, hard execution deadlines, and recoverable
file/command output pagination. The root go.mod selects this reviewed local
copy. Windows Job Object enforcement is host policy; it is not a security
sandbox and does not restrict network access. Unix requires deployment-level
memory/CPU limits in addition to the process-group cleanup provided here.

The 2026-10-28 framework review additionally maintains:

- Bounded SSE events, total streams, JSON responses and error bodies. Provider
  EOF without a terminal event is an error, including HTTP 200 error frames.
- Demultiplexed OpenAI tool arguments by call index/item ID. Buffered calls are
  emitted only after a terminal response and valid JSON object arguments.
- Strict harness argument validation; interrupted pending calls are discarded,
  while dispatched calls and their results remain paired in recovery history.
- Transcript writer failures stop model/tool work and cannot trigger failover.
- MCP tool discovery pagination, bounded frames, preservation of structured and
  embedded resource results, and cancellation-aware stdio writes/pending cleanup.

OpenAI tool starts are delayed until the response terminates so interleaved
arguments cannot be sent to the wrong tool. Output limits are explicit failure
boundaries, not a promise to retain arbitrarily large provider responses. MCP
transport failure is propagated because an external action's outcome may be
unknown; cancellation cannot roll back actions already accepted by a server.

Regression fixtures are in llm/integrity_test.go, harness/integrity_test.go and
mcp/integrity_test.go. Preserve these patches and run the root isolated tests
plus this module's tests when rebasing or upgrading upstream Norma.

Second review on 2026-10-28:

- Exhausted output recovery has an explicit output_limit terminal reason.
- Settlement disabled tools are enforced at execution, not only in schemas.
- Tool executors retain their original context/phase; cancellation releases
  queued calls and late completions cannot rewrite emitted interruptions.
- Stopping a Query consumer cancels in-flight tools. Pending progress is bounded
  to 128 messages per tool and 8 KiB previews; final results use separate storage.
- Duplicate/empty tool identities and invalid rewritten arguments are rejected.
- Non-streaming providers require terminal metadata; malformed Anthropic SSE
  data is an error rather than an ignored keepalive.
- MCP missing catalogs/results are distinguished from valid empty collections,
  and malformed result errors propagate to the host execution journal.

The host maps output_limit to exhausted, and only explicit completed results to
done. Additional executor regressions live in harness/review2_test.go.
