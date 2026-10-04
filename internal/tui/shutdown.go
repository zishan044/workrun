package tui

import (
	"sync"

	"github.com/zishan044/workrun/internal/runner"
)

// ShutdownState keeps the first accepted application shutdown reason.
type ShutdownState struct {
	mu     sync.Mutex
	reason runner.StopReason
}

// Request records reason unless shutdown has already started.
func (s *ShutdownState) Request(reason runner.StopReason) (runner.StopReason, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reason != runner.StopNone {
		return s.reason, false
	}
	s.reason = reason
	return reason, true
}

// Reason returns the first accepted shutdown reason.
func (s *ShutdownState) Reason() runner.StopReason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// ShutdownRequestedMsg asks the model to stop and exit after managed work ends.
type ShutdownRequestedMsg struct {
	Reason runner.StopReason
}
