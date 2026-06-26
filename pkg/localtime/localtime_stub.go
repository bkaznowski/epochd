//go:build !linux

// Package localtime injects fake time into local (non-Kubernetes) processes.
// This stub is compiled on non-Linux platforms; all operations return errors.
package localtime

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

var errNotSupported = errors.New("localtime: not supported on this platform (Linux only)")

// Handle holds an active time injection. On non-Linux platforms all operations return errors.
type Handle struct{}

// Session manages fake time for a group of processes. On non-Linux platforms all operations return errors.
type Session struct{}

func Start(_ *exec.Cmd, _ time.Time) (*Handle, error)       { return nil, errNotSupported }
func StartFrozen(_ *exec.Cmd, _ time.Time) (*Handle, error) { return nil, errNotSupported }
func Attach(_ int, _ time.Time) (*Handle, error)            { return nil, errNotSupported }
func AttachFrozen(_ int, _ time.Time) (*Handle, error)      { return nil, errNotSupported }
func (h *Handle) SetTime(_ time.Time) error                 { return errNotSupported }
func (h *Handle) Freeze(_ time.Time) error                  { return errNotSupported }
func (h *Handle) Reset() error                              { return errNotSupported }

func NewSession(_ time.Time) *Session        { return &Session{} }
func (s *Session) Start(_ *exec.Cmd) error   { return errNotSupported }
func (s *Session) Attach(_ int) error        { return errNotSupported }
func (s *Session) SetTime(_ time.Time) error { return errNotSupported }
func (s *Session) Freeze(_ time.Time) error  { return errNotSupported }
func (s *Session) Reset() error              { return errNotSupported }
func (s *Session) Len() int                  { return 0 }

// WithProcess skips the test on non-Linux platforms.
func WithProcess(t *testing.T, _ *exec.Cmd, _ time.Time, _ func(*testing.T, *Handle)) {
	t.Helper()
	t.Skip("localtime: not supported on this platform (Linux only)")
}

// WithPID skips the test on non-Linux platforms.
func WithPID(t *testing.T, _ int, _ time.Time, _ func(*testing.T, *Handle)) {
	t.Helper()
	t.Skip("localtime: not supported on this platform (Linux only)")
}

// WithSession skips the test on non-Linux platforms.
func WithSession(t *testing.T, _ time.Time, _ func(*Session) error, _ func(*testing.T, *Session)) {
	t.Helper()
	t.Skip("localtime: not supported on this platform (Linux only)")
}
