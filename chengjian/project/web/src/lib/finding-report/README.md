# Live finding report renderer

The finding detail page and task attack-chain tab share `AttackChainView` and
`TemplateReport`. The layout is adapted from the local report template; its
static data bundle is not shipped or read by the application.

```ts
const report = mergeReport(normalizeReport(finding));
```

`normalizeReport` parses the live finding's report, evidence and summary without
an LLM or network request. `documents`, `sections` and each block's `source`
retain the original text and exact document/line/character ranges. Unknown
sections, additional PoCs, incomplete fences and long responses remain available
to the renderer. There are no prefix-based deduplication or preview length caps.

Steps are grouped only when an explicit method and path identify a unique
matching step. All remaining content is rendered as original sections or
additional evidence. Request templates preserve explicit methods, schemes,
headers and bodies. A missing method or host is not guessed. Having a response
in the report never implies independent verification. Upstream omission markers
are preserved and labelled; missing original bytes cannot be reconstructed.

The report fills the available page width. The fixed chain selects the matching
step in the evidence reader. A sticky step directory, active-step indicator and
previous/next controls support review; small screens use a step selector. Only
the selected step mounts, and switching back restores the complete source text.
Long or unbalanced request/response pairs stack at full width. Long PoCs (over 24
lines or 1800 characters) and the
root-cause, remediation and supplemental sections start closed; their content
mounts only when the user opens the section. Expanding them exposes the complete
original content, without preview caps. Source buttons open
the original database document. Related finding navigation uses the server's
scoped exploration-node mapping rather than treating an exploration node number
as a finding ID. Captured traffic is rendered separately by `ExpandedTraffic`,
which reads complete text-body pagination and offers downloads for binary bodies.

Run synthetic regression checks from `web/`:

```sh
node scripts/check-finding-report.cjs
node --test src/lib/finding-report/parser.test.mjs
```

These checks use synthetic reports and never execute PoCs or modify findings.

The integrated report presentation uses a warm paper background (`#f4f3ef`) and
muted blue-gray investigation/code fields (`#e5e9eb`). The shared renderer is
`components/attack-chain/template-report.tsx`; its CSS module owns semantic
colors, outline behavior icons, responsive directories, and reduced-motion
support. Both finding detail and task investigations consume this renderer.
The approved standalone reference remains in
`E:/cyberagent/design/finding-report-taste-demo/` (port 8803).

The fixed behavior path follows the approved preview interaction: behavior nodes
select their matching evidence step; an explicit source node opens its stored
source document. The evidence directory and previous/next controls switch in
place without scrolling the document. Additional behavior indexes and generic
phase popovers are omitted. Conditions and results can be explored only when an
explicit four-column capture exists in the report; original captures remain
available on demand. No responses or dependencies are generated. Long code fields
scroll internally at 390px and retain all saved bytes for reading and copying.
Chapter navigation only includes sections actually present. Raw saved traffic
shares that navigation and retains the original pagination/download behavior.
Motion highlights only existing records and never initiates a test request.
