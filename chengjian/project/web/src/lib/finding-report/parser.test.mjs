import { classifyBlock, extractBlocks, mergeReport, normalizeReport, toCurl } from "./parser.ts";
import assert from "node:assert/strict";
import test from "node:test";

test("source sections partition every byte, including duplicate/unknown headings and fences", () => {
  const report =
    '# Title\r\nIntro untouched\r\n## 概述\r\nfirst\r\nsecond\r\n## 概述\r\nthird\r\n## Custom\r\n~~~json\r\n{"x":1}\r\n## still code\r\n~~~\r\n````text\r\n```\r\n# not heading\r\n````\r\n```unclosed\r\nlast line';
  const normalized = normalizeReport({ id: "31", report });
  assert.equal(normalized.rawMarkdown, report);
  assert.equal(normalized.documents[0].sections.map((section) => section.raw).join(""), report);
  assert(normalized.verdict.statement.includes("first\r\nsecond"));
  assert(normalized.verdict.statement.includes("third"));
  assert.equal(normalized.unrecognizedSections.length, 1);
  assert.equal(normalized.documents[0].blocks.length, 3);
  assert.equal(normalized.documents[0].blocks.at(-1).closed, false);
  assert.equal(normalized.documents[0].blocks.at(-1).body, "last line");
  for (const block of normalized.documents[0].blocks) {
    assert.equal(report.slice(block.source.start, block.source.end), block.source.text);
    assert.equal(report.split("\n")[block.source.line - 1], block.source.text.split("\n")[0]);
  }
});

test("matching long responses are never deduplicated by their first 400 characters", () => {
  const shared = "A".repeat(500);
  const report = mergeReport({
    id: 1,
    report: `## 证据\n**A**\n\`\`\`http\nHTTP/1.1 200 OK\n\n${shared}FIRST_END\n\`\`\`\n**B**\n\`\`\`http\nHTTP/1.1 200 OK\n\n${shared}SECOND_END\n\`\`\``,
  });
  assert.equal(report.mergedSteps.flatMap((step) => step.response).length, 2);
  assert.equal(report.verification.verified, false);
  assert.equal(report.verify.raw, "");
});

test("all code and inline paragraphs remain represented in steps", () => {
  const report = mergeReport({
    id: 1,
    report:
      "## 复现步骤\n前置条件不能丢。\n**步骤 1：运行**\n> 这是一条限制。\n```python\nimport sys\nprint('complete')\n```\n## 修复建议\n先评估。\n- 一项\n  连续的补充内容\n\n最后一段也保留。",
    evidence: "Standalone\n1) Compare\nno code fences\n2) Check\noriginal evidence",
  });
  assert(report.mergedSteps.some((step) => step.notes.some((note) => note.includes("前置条件不能丢"))));
  assert(report.mergedSteps.some((step) => step.notes.some((note) => note.includes("> 这是一条限制"))));
  assert(report.mergedSteps.some((step) => step.other.some((block) => block.body.includes("import sys"))));
  assert(report.remediation.join("\n").includes("最后一段也保留。"));
  assert(report.mergedSteps.some((step) => step.sources.some((source) => source.document === "evidence")));
});

test("response excerpts are flagged as upstream and never expanded", () => {
  const md = '## 证据\n```http\nHTTP/1.1 200 OK\n\n{"data":[1,...]}\n```';
  const report = mergeReport({ id: 1, report: md });
  const response = report.mergedSteps.flatMap((step) => step.response)[0];
  assert.equal(response.excerpted, true);
  assert(response.body.includes("[1,...]"));
  assert.equal(report.verification.verified, false);
  assert(report.identity.warnings.some((warning) => warning.includes("上游摘录")));
});

test("curl method and scheme are explicit; raw HTTP is never guessed from shell syntax", () => {
  const report = mergeReport({
    id: 1,
    report: "## 复现步骤\n```bash\ncurl -X DELETE 'http://example.invalid:8080/item?q=1' -H 'X-Test: source'\n```",
  });
  assert.equal(report.verify.template.method, "DELETE");
  assert.equal(report.verify.template.scheme, "http");
  assert.equal(report.verify.curlKind, "original");
  assert(report.verify.curl.includes("http://example.invalid:8080"));
  assert.equal(report.verify.rawKind, "unavailable");
  assert.equal(report.verify.raw, "");
  const unspecified = mergeReport({ id: 2, report: "## 复现步骤\n```bash\ncurl http://example.invalid/path\n```" });
  assert.equal(unspecified.verify.template.method, "");
  assert(unspecified.verify.template.missing.includes("method"));
  const scopeOnly = mergeReport({ id: 3, report: "## 受影响范围\n`/only-a-path`" });
  assert.equal(scopeOnly.verify.template.method, "");
  assert.equal(scopeOnly.verify.curl, "");
  assert.equal(scopeOnly.verify.raw, "");
});

test("raw header parsing stops at body; no scheme or headers are invented", () => {
  const report = mergeReport({
    id: 1,
    report:
      "## 复现步骤\n```http\nPOST /resource HTTP/1.1\nHost: example.invalid\nX-Test: from-header\n\nX-Test: from-body\n```",
  });
  assert.deepEqual(report.verify.template.headers, [
    ["Host", "example.invalid"],
    ["X-Test", "from-header"],
  ]);
  assert(report.verify.template.body.includes("X-Test: from-body"));
  assert.equal(report.verify.template.scheme, "");
  assert.equal(report.verify.curl, "");
  assert(!report.verify.raw.includes("Content-Length:"));
});

test("JSON and executable code are not classified as a verified HTTP response", () => {
  assert.equal(classifyBlock('{"code":"test-request"}', "json"), "json");
  assert.equal(classifyBlock("signature = hmac.new(key,data)", "python"), "code");
  assert.equal(extractBlocks("```python\nsignature = hmac.new(key,data)\n```")[0].kind, "code");
});

test("method and complete request target identify associations; node references retain their namespace", () => {
  const report = mergeReport({
    id: 79,
    report:
      "## 复现步骤\n**1. Read**\nGET /data?q=1\n**2. Modify**\nPOST /data?q=1\n## 证据\n**Read**\nGET /data?q=1\n**Modify**\nPOST /data?q=1\nfinding79/80 fact326、331 intent328",
  });
  assert.equal(report.mergedSteps.length, 2);
  assert(report.mergedSteps.every((step) => step.match === "exact-endpoint"));
  assert.deepEqual(report.relations.findings, [79, 80]);
  assert.deepEqual(report.relations.facts, [326, 331]);
  assert.equal(report.relations.namespace, "exploration-node");
});

test("derived curl shell-quotes exact fields and labels itself derived", () => {
  const verify = mergeReport({
    id: 1,
    report: "## 复现步骤\n```http\nPOST http://example.invalid/a HTTP/1.1\nX-Name: O'Reilly\n\nsource body\n```",
  }).verify;
  assert.equal(verify.curlKind, "derived");
  assert(verify.curl.includes("# 派生文本"));
  assert(verify.curl.includes("O'\\''Reilly"));
  assert(!verify.curl.includes("https://"));
  assert.equal(toCurl({ ...verify.template, scheme: "", path: "/a" }), "");
});
