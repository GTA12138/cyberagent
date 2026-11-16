package server

import (
	"os"
	"path/filepath"

	actool "github.com/Autumn-27/norma/tool"
)

// Retain bytes before rendering a small model preview. The file is readable by
// the same agent's Read tool, including byte paging and base64 for binary data.
func preserveToolBody(tc *actool.ToolContext, data []byte) (string, error) {
	dir := filepath.Join(os.TempDir(), "chengjian-tool-output")
	if tc != nil {
		if tc.OutputDir != "" {
			dir = tc.OutputDir
		} else if tc.WorkingDir != "" {
			dir = filepath.Join(tc.WorkingDir, "cmd-output")
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, "response-*.body")
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return f.Name(), nil
}
