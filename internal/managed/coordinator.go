package managed

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type control struct {
	Generation string `json:"generation"`
	Committed  bool   `json:"committed,omitempty"`
}

// Prepare is called while holding locks/claude.lock. A false result is a
// pending drain: callers must retain the generation and poll on later ticks.
// The durable participant set prevents a crashed runner from looking exited.
func Prepare(dataDir, generation string, blockers []int) (bool, error) {
	if generation == "" || len(generation) > 1024 {
		return false, errors.New("invalid managed Claude generation")
	}
	root, err := rootDir(dataDir)
	if err != nil {
		return false, err
	}
	path := filepath.Join(root, "pending.json")
	var txn transaction
	if err := readJSON(path, &txn); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if txn.Generation != "" && txn.Generation != generation {
		if err := Release(dataDir, "", false); err != nil {
			return false, err
		}
		txn = transaction{}
	}
	live, stale, err := scan(root)
	if err != nil {
		return false, err
	}
	for _, state := range stale {
		if state.Generation != "" {
			return false, fmt.Errorf("managed Claude runner %s stopped during generation %s; manual recovery required", state.ID, state.Generation)
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
	for _, pid := range blockers {
		if !byPID[pid] {
			return false, fmt.Errorf("Claude process %d is not a live managed child", pid)
		}
	}
	for _, id := range txn.IDs {
		if _, ok := byID[id]; !ok {
			return false, fmt.Errorf("managed Claude runner %s disappeared during rotation; manual recovery required", id)
		}
	}
	if txn.Generation == "" {
		txn.Generation = generation
		for _, p := range live {
			txn.IDs = append(txn.IDs, p.reg.ID)
		}
		if err := atomicJSON(path, txn); err != nil {
			return false, err
		}
	} else {
		known := make(map[string]bool, len(txn.IDs))
		for _, id := range txn.IDs {
			known[id] = true
		}
		for _, p := range live {
			if !known[p.reg.ID] {
				return false, errors.New("new managed Claude runner appeared during rotation")
			}
		}
	}
	allReady := true
	for _, id := range txn.IDs {
		p := byID[id]
		var state StatusInfo
		if err := request(p.reg, "/prepare", control{Generation: generation}, &state); err != nil {
			return false, err
		}
		if state.Generation != generation || (!state.Exited && (!state.Ready || state.SessionID == "")) {
			allReady = false
		}
		if state.Recovery != "" {
			return false, errors.New(state.Recovery)
		}
	}
	if !allReady {
		return false, nil
	}
	for _, id := range txn.IDs {
		p := byID[id]
		var state StatusInfo
		if err := request(p.reg, "/exit", control{Generation: generation}, &state); err != nil {
			return false, err
		}
		if state.Recovery != "" {
			return false, errors.New(state.Recovery)
		}
	}
	// Only Wait-confirmed child exits count; a /exiting acknowledgement does not.
	for _, id := range txn.IDs {
		var state StatusInfo
		if err := request(byID[id].reg, "/status", nil, &state); err != nil {
			return false, err
		}
		if state.Generation != generation || !state.Exited {
			return false, nil
		}
		if state.Recovery != "" {
			return false, errors.New(state.Recovery)
		}
	}
	return true, nil
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
	byID := make(map[string]bool, len(live))
	for _, p := range live {
		byID[p.reg.ID] = true
	}
	for _, id := range txn.IDs {
		if !byID[id] {
			return fmt.Errorf("managed Claude runner %s disappeared during rotation; manual recovery required", id)
		}
	}
	for _, state := range stale {
		if state.Generation != "" && (generation == "" || state.Generation == generation) {
			return fmt.Errorf("managed Claude runner %s stopped during generation %s; manual recovery required", state.ID, state.Generation)
		}
	}
	var result error
	for _, p := range live {
		if p.status.Generation == "" {
			continue
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
