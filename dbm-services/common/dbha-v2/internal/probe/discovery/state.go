package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"dbm-services/common/dbha-v2/pkg/process"
	"github.com/google/uuid"
)

type State struct {
	AgentID        string            `json:"agent_id"`
	BootID         string            `json:"boot_id"`
	BootGeneration uint64            `json:"boot_generation"`
	SessionID      string            `json:"session_id,omitempty"`
	SessionEpoch   uint64            `json:"session_epoch,omitempty"`
	Sequences      map[string]uint64 `json:"sequences,omitempty"`
}

// StartBoot advances the durable generation before contacting the server.
// A missing state file is permitted only for a genuinely new local agent.
func StartBoot(path, agentID string) (State, error) {
	var s State
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return s, err
	}
	lockPath, err := process.LockPathFor(path)
	if err != nil {
		return s, err
	}
	lock, err := process.AcquireFileLock(lockPath, 10*time.Second)
	if err != nil {
		return s, err
	}
	defer lock.Unlock()
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &s); err != nil {
			return s, err
		}
		if s.AgentID != agentID || s.BootGeneration == 0 {
			return s, fmt.Errorf("agent identity state mismatch")
		}
	} else if !os.IsNotExist(err) {
		return s, err
	}
	s.AgentID, s.BootID = agentID, uuid.NewString()
	s.BootGeneration++
	s.SessionID, s.SessionEpoch = "", 0
	s.Sequences = map[string]uint64{}
	if err := saveLocked(path, s); err != nil {
		return s, err
	}
	return s, nil
}

func SaveState(path string, s State) error {
	lockPath, err := process.LockPathFor(path)
	if err != nil {
		return err
	}
	lock, err := process.AcquireFileLock(lockPath, 10*time.Second)
	if err != nil {
		return err
	}
	defer lock.Unlock()
	return saveLocked(path, s)
}

func saveLocked(path string, s State) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil && !os.IsExist(err) {
			return err
		}
		if f != nil {
			_ = f.Close()
		}
	} else if err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = process.WriteFileLocked(path, b)
	return err
}
