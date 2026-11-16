# Finding attack chains

The finding detail defaults to an investigation graph; the task detail also has an
attack-chain tab. Nodes use monochrome behavior icons rather than colored cards.
The right-hand inspector contains source excerpts and document line numbers.
Selecting a node or an edge expands and locates the corresponding evidence in the
inspector. “定位原始证据” opens the complete database document with the source range
highlighted. References open their stored exploration record, and execution logs
load the complete activity detail after verifying its chain membership.

Evidence locations can be opened independently and shared via finding detail URLs.
Shared links include a document change detector; if the stored content changes,
the viewer avoids highlighting an outdated location. Closing the viewer returns
to the chain without losing the current investigation context.

The read-only `GET /api/exploration/findings/{id}/attack-chain` endpoint structures
stored summaries, report sections, numbered steps, evidence, and explicit node
references. It does not execute requests or call a model. A document without
independent steps becomes a single evidence record, not an invented timeline.

Solid edges indicate document order. Dashed edges indicate report references,
not independently proven prerequisites. References such as `finding79` resolve
in the originating exploration's node namespace before mapping to a standalone
finding ID. Task context uses the existing finding provenance rules.

Every excerpt identifies its original document and line where available. Missing
analysis, missing files, and missing HTTP snapshots remain visible as gaps. Tool
results belong to the exploration intent that yielded the finding; they are not
presented as individually matched step evidence. No database migration is needed.

Design references:

- [Elastic visual event analyzer](https://www.elastic.co/docs/solutions/security/investigate/visual-event-analyzer): event exploration, semantic icons, and an adjacent inspector.
- [CrowdStrike attack path analysis](https://www.crowdstrike.com/en-us/platform/exposure-management/attack-path-analysis/): connected exposure investigation and path context.

Verification: `go test ./attackchain`, a production Next.js static build, Go build
with `-tags embedui`, and authenticated browser/API checks against stored findings.
