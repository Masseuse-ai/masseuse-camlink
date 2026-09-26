package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// StateFile is the updater's memory in the state directory.
const StateFile = "update.json"

// State is what the updater remembers between runs and across a restart.
type State struct {
	// LastCheck is when the latest release was last looked at.
	LastCheck time.Time `json:"lastCheck,omitempty"`
	// Staged is a verified download waiting to be installed, if any.
	Staged *Staged `json:"staged,omitempty"`
	// Failed lists release tags whose verification or installation failed
	// and when, so one is not tried again for a day.
	Failed map[string]time.Time `json:"failed,omitempty"`
	// Previous is the install the last update moved aside (a bundle under
	// the state directory's previous/, .previous/ for the other layouts:
	// Installer.PreviousName), removed once the new program has proven
	// itself; From is the tag it was.
	Previous string `json:"previous,omitempty"`
	From     string `json:"from,omitempty"`
	// Installed is the tag the last update put in place and when: the new
	// program says so once and clears it.
	Installed   string    `json:"installed,omitempty"`
	InstalledAt time.Time `json:"installedAt,omitempty"`
}

// LoadState reads the state file; a missing or unreadable one is empty.
func LoadState(stateDir string) *State {
	var s State
	b, err := os.ReadFile(filepath.Join(stateDir, StateFile))
	if err != nil || json.Unmarshal(b, &s) != nil {
		return &State{}
	}
	return &s
}

// Save writes the state file.
func (s *State) Save(stateDir string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	tmp := filepath.Join(stateDir, StateFile+".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(stateDir, StateFile))
}

// FailedRecently says whether tag failed within the last day, so it is
// left alone until then (a bad release is not downloaded every six hours).
func (s *State) FailedRecently(tag string, now time.Time) bool {
	at, ok := s.Failed[tag]
	return ok && now.Sub(at) < 24*time.Hour
}

// MarkFailed remembers a failure of tag.
func (s *State) MarkFailed(tag string, now time.Time) {
	if s.Failed == nil {
		s.Failed = map[string]time.Time{}
	}
	s.Failed[tag] = now
	for t, at := range s.Failed {
		if now.Sub(at) > 7*24*time.Hour {
			delete(s.Failed, t)
		}
	}
}

// ErrNoPrevious says there is no previous install to remove.
var ErrNoPrevious = errors.New("update: no previous install")

// RemovePrevious deletes the install moved aside by the last update, once
// the new program has proven itself (its first successful poll of the
// service). Windows may refuse while the old program is still exiting;
// the caller retries.
func (s *State) RemovePrevious() error {
	if s.Previous == "" {
		return ErrNoPrevious
	}
	if err := os.RemoveAll(s.Previous); err != nil {
		return err
	}
	s.Previous, s.From = "", ""
	return nil
}
