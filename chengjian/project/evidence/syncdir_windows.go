//go:build windows

package evidence

// Windows does not support FlushFileBuffers on directory handles. The body is
// already flushed and closed before Rename; attempting os.File.Sync on its
// parent returns ACCESS_DENIED and would reject every new evidence binding.
func syncEvidenceDir(string) error { return nil }
