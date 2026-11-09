package managed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type control struct {
	Generation string `json:"generation"`
	Committed  bool   `json:"committed,omitempty"`
}

// remainingIDs keeps the participants that are still running. A runner that
// ended without a generation after its Claude exited has nothing to drain: it
// never joined this rotation (for example, the user closed a session whose
// terminal could not be verified), was released from it, or saw its Claude
// end before the exit it was asked for (the user's own /exit, a hang-up, a
// crash, or the runner itself stopped). Such a runner removes its directory,
// or keeps a record for the user. Any other lost participant may still own a
// session and needs manual recovery.
func remainingIDs(ids []string, live []participant, stale []StatusInfo) ([]string, error) {
	running := make(map[string]bool, len(live))
	for _, p := range live {
		running[p.reg.ID] = true
	}
	lost := make(map[string]bool, len(stale))
	for _, state := range stale {
		lost[state.ID] = state.Generation != "" || !state.Exited
	}
	var kept []string
	for _, id := range ids {
		if running[id] {
			kept = append(kept, id)
		} else if lost[id] {
			return nil, fmt.Errorf("managed Claude runner %s disappeared during rotation; manual recovery required", id)
		}
	}
	return kept, nil
}

// processExists reports whether pid names a process. One owned by another
// user counts, and so does a reused pid: when unsure, the switch stops.
func processExists(pid int) bool {
	err := unix.Kill(pid, 0)
	return err == nil || errors.Is(err, unix.EPERM)
}

// Waiting is a session a switch still waits for, with the reason it gives.
type Waiting struct {
	SessionID string
	Reason    string
}

// The reasons of a session whose Claude is on its way out: the exit was
// acknowledged, or its deadline passed and the runner is ending the child.
// Every other reason describes a busy session.
const (
	ReasonExiting     = "exiting"
	ReasonExitOverdue = "exit overdue, ending it"
)

// Exiting reports whether reason is one of a session on its way out.
func Exiting(reason string) bool {
	return reason == ReasonExiting || reason == ReasonExitOverdue
}

// Prepare is called while holding locks/claude.lock. A false result is a
// pending drain: callers must retain the generation and poll on later ticks,
// and the sessions it still waits for are listed. The durable participant set
// prevents a crashed runner from looking exited.
func Prepare(dataDir, generation string, blockers []int) (bool, []Waiting, error) {
	if generation == "" || len(generation) > 1024 {
		return false, nil, errors.New("invalid managed Claude generation")
	}
	root, err := rootDir(dataDir)
	if err != nil {
		return false, nil, err
	}
	path := filepath.Join(root, "pending.json")
	var txn transaction
	if err := readJSON(path, &txn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, nil, err
	}
	if txn.Generation != "" && txn.Generation != generation {
		if err := Release(dataDir, "", false); err != nil {
			return false, nil, err
		}
		txn = transaction{}
	}
	live, stale, err := scan(root)
	if err != nil {
		return false, nil, err
	}
	for _, state := range stale {
		if state.Generation != "" {
			return false, nil, fmt.Errorf("managed Claude runner %s stopped during generation %s; manual recovery required", state.ID, state.Generation)
		}
	}
	byID := make(map[string]participant, len(live))
	byPID := make(map[int]bool, len(live))
	for _, p := range live {
		byID[p.reg.ID] = p
		if !p.status.Exited && p.status.ChildPID > 0 {
			byPID[p.status.ChildPID] = true
		}
	}
	// The caller read the blockers before this scan, and the quota check it
	// runs in between can take seconds. A child that finished its exit in
	// that time is gone and holds nothing; failing on it cancelled the drain
	// and restarted every session that had already exited (11-08 06:54).
	// Only a Claude that still runs outside the live runners, as one left
	// by a killed runner, stops the switch.
	for _, pid := range blockers {
		if !byPID[pid] && processExists(pid) {
			return false, nil, fmt.Errorf("claude process %d is not a live managed child", pid)
		}
	}
	if txn.IDs, err = remainingIDs(txn.IDs, live, stale); err != nil {
		return false, nil, err
	}
	if txn.Generation == "" {
		txn.Generation = generation
		for _, p := range live {
			txn.IDs = append(txn.IDs, p.reg.ID)
		}
		if err := atomicJSON(path, txn); err != nil {
			return false, nil, err
		}
	} else {
		known := make(map[string]bool, len(txn.IDs))
		for _, id := range txn.IDs {
			known[id] = true
		}
		for _, p := range live {
			if !known[p.reg.ID] {
				return false, nil, errors.New("new managed Claude runner appeared during rotation")
			}
		}
	}
	var waiting []Waiting
	for _, id := range txn.IDs {
		p := byID[id]
		var state StatusInfo
		if err := request(p.reg, "/prepare", control{Generation: generation}, &state); err != nil {
			return false, nil, err
		}
		if state.Generation != generation || (!state.Exited && (!state.Ready || state.SessionID == "")) {
			reason := state.Reason
			if reason == "" {
				reason = "not ready"
			}
			waiting = append(waiting, Waiting{SessionID: state.SessionID, Reason: reason})
		}
		if state.Recovery != "" {
			return false, nil, errors.New(state.Recovery)
		}
	}
	if len(waiting) > 0 {
		return false, waiting, nil
	}
	for _, id := range txn.IDs {
		p := byID[id]
		var state StatusInfo
		if err := request(p.reg, "/exit", control{Generation: generation}, &state); err != nil {
			return false, nil, err
		}
		if state.Recovery != "" {
			return false, nil, errors.New(state.Recovery)
		}
	}
	// Only Wait-confirmed child exits count; a /exiting acknowledgement does not.
	for _, id := range txn.IDs {
		var state StatusInfo
		if err := request(byID[id].reg, "/status", nil, &state); err != nil {
			return false, nil, err
		}
		if state.Generation != generation || !state.Exited {
			reason := ReasonExiting
			if Exiting(state.Reason) {
				reason = state.Reason
			}
			waiting = append(waiting, Waiting{SessionID: state.SessionID, Reason: reason})
			continue
		}
		if state.Recovery != "" {
			return false, nil, errors.New(state.Recovery)
		}
	}
	if len(waiting) > 0 {
		return false, waiting, nil
	}
	return true, nil, nil
}

// Release cancels draining or authorizes restarting sessions after a committed
// credential switch. An empty generation cancels every outstanding generation.
// The caller holds locks/claude.lock; runners acquire it asynchronously before
// starting children, after this function returns and the caller unlocks.
func Release(dataDir, generation string, committed bool) error {
	if generation == "" && committed {
		return errors.New("committed release requires a generation")
	}
	root, err := rootDir(dataDir)
	if err != nil {
		return err
	}
	var txn transaction
	path := filepath.Join(root, "pending.json")
	if err := readJSON(path, &txn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if generation != "" && txn.Generation != "" && txn.Generation != generation {
		return errors.New("managed Claude release generation mismatch")
	}
	live, stale, err := scan(root)
	if err != nil {
		return err
	}
	if _, err := remainingIDs(txn.IDs, live, stale); err != nil {
		return err
	}
	for _, state := range stale {
		if state.Generation != "" && (generation == "" || state.Generation == generation) {
			return fmt.Errorf("managed Claude runner %s stopped during generation %s; manual recovery required", state.ID, state.Generation)
		}
	}
	var result error
	for _, p := range live {
		if p.status.Generation == "" || p.status.Generation == upgradeGeneration {
			continue // a runner upgrade is not a daemon generation
		}
		if generation != "" && p.status.Generation != generation {
			continue
		}
		if err := request(p.reg, "/release", control{Generation: p.status.Generation, Committed: committed}, nil); err != nil {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return result
	}
	if txn.Generation != "" && (generation == "" || generation == txn.Generation) {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
