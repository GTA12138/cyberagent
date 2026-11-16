export interface WorkspaceDraft {
  id: number;
  content: string;
  dirty: boolean;
  saving: boolean;
}

/** A save acknowledges exactly the submitted draft, even if editing continued. */
export function finishDraftSave<T extends WorkspaceDraft>(
  current: T | null,
  saved: Pick<WorkspaceDraft, "id" | "content">,
  succeeded: boolean,
): T | null {
  if (!current || current.id !== saved.id) return current;
  return {
    ...current,
    saving: false,
    dirty: succeeded ? current.content !== saved.content : current.dirty,
  };
}
