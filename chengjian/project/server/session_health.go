package server

import (
	"context"
	"errors"
)

type sessionProbeState string

const (
	sessionProbeAlive   sessionProbeState = "alive"
	sessionProbeDead    sessionProbeState = "dead"
	sessionProbeUnknown sessionProbeState = "unknown"
)

// A canceled local wait is not evidence that the remote endpoint has died.
// Inspect both the operation context and typed causes: some drivers return a
// wrapped timeout before the caller's longer context expires.
func classifySessionProbe(ctxErr, probeErr error) sessionProbeState {
	if ctxErr != nil || errors.Is(probeErr, context.Canceled) || errors.Is(probeErr, context.DeadlineExceeded) {
		return sessionProbeUnknown
	}
	if probeErr != nil {
		return sessionProbeDead
	}
	return sessionProbeAlive
}
