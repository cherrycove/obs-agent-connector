package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var ErrAlreadyCompleted = errors.New("turn already completed")

type Manager struct {
	Root       string
	StaleAfter time.Duration
	Now        func() time.Time
}

type Claim struct {
	dir      string
	lockFile string
}

type claimRecord struct {
	StartedAt   string `json:"started_at"`
	Fingerprint string `json:"fingerprint,omitempty"`
	PID         int    `json:"pid"`
}

func (m Manager) Completed(sessionID, turnID string) (bool, error) {
	if strings.TrimSpace(m.Root) == "" {
		return false, errors.New("state root is empty")
	}
	_, err := os.Stat(filepath.Join(m.turnDir(sessionID, turnID), "completed.json"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (m Manager) turnDir(sessionID, turnID string) string {
	key := sha256.Sum256([]byte(sessionID + "\x00" + turnID))
	return filepath.Join(m.Root, hex.EncodeToString(key[:]))
}

func (m Manager) Claim(sessionID, turnID, fingerprint string) (*Claim, error) {
	if strings.TrimSpace(m.Root) == "" {
		return nil, errors.New("state root is empty")
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	staleAfter := m.StaleAfter
	if staleAfter <= 0 {
		staleAfter = 10 * time.Minute
	}
	dir := m.turnDir(sessionID, turnID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "completed.json")); err == nil {
		return nil, ErrAlreadyCompleted
	}
	lockFile := filepath.Join(dir, "claim.json")
	record := claimRecord{
		StartedAt:   now().UTC().Format(time.RFC3339Nano),
		Fingerprint: fingerprint,
		PID:         os.Getpid(),
	}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		file, openErr := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if openErr == nil {
			if _, err := file.Write(body); err != nil {
				_ = file.Close()
				_ = os.Remove(lockFile)
				return nil, err
			}
			if err := file.Close(); err != nil {
				_ = os.Remove(lockFile)
				return nil, err
			}
			return &Claim{dir: dir, lockFile: lockFile}, nil
		}
		if !os.IsExist(openErr) {
			return nil, openErr
		}
		info, statErr := os.Stat(lockFile)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return nil, statErr
		}
		startedAt := info.ModTime()
		if existing, readErr := os.ReadFile(lockFile); readErr == nil {
			var existingRecord claimRecord
			if jsonErr := json.Unmarshal(existing, &existingRecord); jsonErr == nil {
				if parsed, parseErr := time.Parse(time.RFC3339Nano, existingRecord.StartedAt); parseErr == nil {
					startedAt = parsed
				}
			}
		}
		if now().Sub(startedAt) <= staleAfter {
			return nil, nil
		}
		if err := os.Remove(lockFile); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, nil
}

func (c *Claim) SignalWasUploaded(signal string) bool {
	if c == nil || !validSignal(signal) {
		return false
	}
	_, err := os.Stat(filepath.Join(c.dir, signal+".json"))
	return err == nil
}

func (c *Claim) MarkSignalUploaded(signal string, details map[string]any) error {
	if c == nil {
		return errors.New("claim is nil")
	}
	if !validSignal(signal) {
		return fmt.Errorf("unsupported signal %q", signal)
	}
	payload := map[string]any{
		"signal":      signal,
		"uploaded_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	for key, value := range details {
		if key == "headers" || key == "authorization" || key == "token" {
			continue
		}
		payload[key] = value
	}
	return writeJSONAtomic(filepath.Join(c.dir, signal+".json"), payload)
}

func (c *Claim) Complete(requiredSignals ...string) error {
	if c == nil {
		return errors.New("claim is nil")
	}
	for _, signal := range requiredSignals {
		if !validSignal(signal) {
			return fmt.Errorf("unsupported signal %q", signal)
		}
		if !c.SignalWasUploaded(signal) {
			return fmt.Errorf("signal %s has not been uploaded", signal)
		}
	}
	if err := writeJSONAtomic(filepath.Join(c.dir, "completed.json"), map[string]any{
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		return err
	}
	return c.Release()
}

func (c *Claim) Release() error {
	if c == nil || c.lockFile == "" {
		return nil
	}
	err := os.Remove(c.lockFile)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func validSignal(value string) bool {
	return value == "traces" || value == "metrics"
}

func writeJSONAtomic(path string, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
