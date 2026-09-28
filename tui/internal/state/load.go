package state

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// ErrNeedsUpgrade is returned by Load/Parse when the snapshot has no
// "schema" field at all, meaning it was written by a runnermaxxer.sh older
// than the version that introduced state.json (< 3.1.0).
type ErrNeedsUpgrade struct{}

func (ErrNeedsUpgrade) Error() string {
	return "state.json has no \"schema\" field: needs runnermaxxer.sh >= 3.1.0"
}

// ErrSchemaTooNew is returned when the snapshot declares a schema version
// higher than this client understands.
type ErrSchemaTooNew struct {
	Got       int
	Supported int
}

func (e ErrSchemaTooNew) Error() string {
	return fmt.Sprintf("snapshot schema v%d is newer than this client supports (v%d) - upgrade runnermaxxer-tui", e.Got, e.Supported)
}

// Load reads and parses a state.json file from disk.
func Load(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}
	return Parse(data, SourceDaemonFile)
}

// Parse decodes raw JSON bytes into a Snapshot, applying the loader rules:
// unknown fields are ignored, a missing "schema" field means the writer
// predates schema 2, a schema newer than SupportedSchema is rejected, a
// missing/zero refresh_interval defaults to 5, and empty target labels are
// derived from the URL the way runnermaxxer.sh's target_label does.
func Parse(data []byte, src Source) (Snapshot, error) {
	var presence map[string]json.RawMessage
	if err := json.Unmarshal(data, &presence); err != nil {
		return Snapshot{}, fmt.Errorf("parsing state.json: %w", err)
	}
	if raw, ok := presence["schema"]; !ok || len(raw) == 0 || string(raw) == "null" {
		return Snapshot{}, ErrNeedsUpgrade{}
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("parsing state.json: %w", err)
	}
	if snap.Schema > SupportedSchema {
		return Snapshot{}, ErrSchemaTooNew{Got: snap.Schema, Supported: SupportedSchema}
	}
	if snap.RefreshInterval <= 0 {
		snap.RefreshInterval = 5
	}
	for i := range snap.Targets {
		t := &snap.Targets[i]
		if t.Label == "" {
			t.Label = deriveLabel(t.URL, t.Type)
		}
	}
	snap.LoadedAt = time.Now()
	snap.Source = src
	return snap, nil
}

// deriveLabel mirrors runnermaxxer.sh's target_label: a repo URL becomes
// "owner/repo", an org URL becomes "org (org)".
func deriveLabel(url, typ string) string {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	trimmed = strings.TrimPrefix(trimmed, "https://")
	trimmed = strings.TrimPrefix(trimmed, "http://")
	trimmed = strings.TrimPrefix(trimmed, "github.com/")
	trimmed = strings.Trim(trimmed, "/")
	if trimmed == "" {
		return url
	}
	if typ == "org" {
		return fmt.Sprintf("%s (org)", trimmed)
	}
	return trimmed
}
