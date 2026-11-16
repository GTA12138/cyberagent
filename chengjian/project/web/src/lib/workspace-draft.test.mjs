import assert from "node:assert/strict";
import test from "node:test";
import { finishDraftSave } from "./workspace-draft.ts";

test("typing while a save is in flight keeps the newer draft unsaved", () => {
  const submitted = { id: 1, content: "first version" };
  const current = { ...submitted, content: "second version", dirty: true, saving: true };
  assert.deepEqual(finishDraftSave(current, submitted, true), {
    ...current, dirty: true, saving: false,
  });
  assert.equal(finishDraftSave({ ...current, content: submitted.content }, submitted, true).dirty, false);
});

test("late saves cannot acknowledge a different or reopened document", () => {
  const current = { id: 2, content: "new document", dirty: true, saving: true };
  assert.equal(finishDraftSave(current, { id: 1, content: "old document" }, true), current);
  assert.equal(finishDraftSave(null, { id: 1, content: "closed" }, true), null);
});

test("failed saves preserve edits for retry", () => {
  const current = { id: 1, content: "draft", dirty: true, saving: true };
  assert.deepEqual(finishDraftSave(current, current, false), { ...current, saving: false });
});
