package managed

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("managed Claude sessions are supported on Linux and macOS, not Windows")
var ErrRunnerUnavailable = errors.New("runner did not answer")

type Options struct {
	DataDir             string
	ClaudeBin           string
	Args                []string
	InitialContinuation string
}

type StatusInfo struct {
	ID            string `json:"id"`
	RunnerPID     int    `json:"runner_pid"`
	ChildPID      int    `json:"child_pid"`
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	Ready         bool   `json:"ready"`
	NeedsContinue bool   `json:"needs_continue"`
	Exited        bool   `json:"exited"`
	Generation    string `json:"generation"`
	Reason        string `json:"reason"`
	Recovery      string `json:"recovery,omitempty"`
	Live          bool   `json:"live"`
}

type Waiting struct{ SessionID, Reason string }

func Run(context.Context, Options) error                        { return errUnsupported }
func Status(string) ([]StatusInfo, error)                       { return nil, errUnsupported }
func Forget(string, string, func(int) bool) (StatusInfo, error) { return StatusInfo{}, errUnsupported }
func TakeLimitHit(string) bool                                  { return false }
func Resume(string) (int, error)                                { return 0, errUnsupported }
func Prepare(string, string, []int) (bool, []Waiting, error)    { return false, nil, errUnsupported }
func Release(string, string, bool) error                        { return errUnsupported }
func Exiting(string) bool                                       { return false }
func ProcessExists(int) bool                                    { return false }
