// Package picker writes the model-discovery cache that Claude Code reads when
// populating its /model picker.
//
// Claude Code will fetch GET {base}/v1/models itself, but only when it has an
// ANTHROPIC_AUTH_TOKEN, an apiKeyHelper or an API key — a plain subscription
// OAuth session does not qualify, and it logs
// "[gatewayDiscovery] skipped: no credential". Setting ANTHROPIC_AUTH_TOKEN to
// satisfy that check costs real features: Claude Code then stops sending the
// 1h prompt-cache ttl and drops several betas.
//
// The read path has no such requirement. Claude Code's reader is:
//
//	function tAe(){ if(!Vg()) return [];
//	  let e = hK(Wg());
//	  if(!e || e.baseUrl !== a.ANTHROPIC_BASE_URL) return []; ... }
//
//	function Vg(){ if(!a.CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY) return false;
//	  if(Pe()!=="firstParty") return false; if(Ko()) return false;
//	  if(!a.ANTHROPIC_BASE_URL) return false; return true }
//
// So writing the cache ourselves gets the picker rows with no credential and no
// loss of fidelity. The schema below was checked against Claude Code 2.1.273's
// validator.
package picker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// MaxModels is the number of entries Claude Code keeps from the cache.
const MaxModels = 100

// CacheFileMode keeps the cache readable only by its owner. It sits next to the
// rest of the user's Claude Code state and lists the models they route.
const CacheFileMode os.FileMode = 0o600

// Cache is the on-disk shape Claude Code validates.
//
// Its parser is `u({baseUrl:o(), fetchedAt:v(), models:k(...)})` over entries
// of `u({id:o(), display_name:o().optional(), description:o().nullish()})`, so
// these three fields are required and unknown ones are stripped.
type Cache struct {
	BaseURL   string  `json:"baseUrl"`
	FetchedAt int64   `json:"fetchedAt"`
	Models    []Model `json:"models"`
}

// Model is one advertised row.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// State describes what Sync did.
type State string

const (
	// StateWrote means the cache was created or replaced.
	StateWrote State = "wrote"
	// StateUnchanged means the file already held these models.
	StateUnchanged State = "unchanged"
	// StateRemoved means the cache file was deleted.
	StateRemoved State = "removed"
	// StateAbsent means there was nothing to remove.
	StateAbsent State = "absent"
)

// Result reports the outcome of a Sync or Remove.
type Result struct {
	State State
	Path  string
	Count int
}

// ConfigDir resolves Claude Code's configuration directory.
func ConfigDir() (string, error) {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// Path returns the cache file Claude Code reads.
func Path() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "cache", "gateway-models.json"), nil
}

// Sync writes the cache unless it already matches.
//
// baseURL must be byte-identical to the ANTHROPIC_BASE_URL Claude Code will
// run with: the reader compares the two with string equality and silently
// ignores the file when they differ.
func Sync(path, baseURL string, models []Model, now time.Time) (Result, error) {
	if baseURL == "" {
		return Result{}, fmt.Errorf("picker cache: base URL is required")
	}
	if len(models) > MaxModels {
		models = models[:MaxModels]
	}
	if models == nil {
		models = []Model{}
	}

	if existing, err := Read(path); err == nil && existing.BaseURL == baseURL && sameModels(existing.Models, models) {
		return Result{State: StateUnchanged, Path: path, Count: len(models)}, nil
	}

	cache := Cache{BaseURL: baseURL, FetchedAt: now.UnixMilli(), Models: models}
	payload, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("picker cache: encode: %w", err)
	}
	payload = append(payload, '\n')

	if err := writeAtomic(path, payload, CacheFileMode); err != nil {
		return Result{}, err
	}
	return Result{State: StateWrote, Path: path, Count: len(models)}, nil
}

// Read loads an existing cache file.
func Read(path string) (*Cache, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cache
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("picker cache: decode %s: %w", path, err)
	}
	return &c, nil
}

// Remove deletes the cache file.
//
// A stale cache is inert — Claude Code ignores one whose baseUrl does not match
// the current ANTHROPIC_BASE_URL — but removing it keeps the picker honest once
// the gateway is no longer in use.
func Remove(path string) (Result, error) {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return Result{State: StateAbsent, Path: path}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("picker cache: remove: %w", err)
	}
	return Result{State: StateRemoved, Path: path}, nil
}

// writeAtomic writes via a temp file in the same directory, so a reader never
// observes a half-written cache, and applies mode before the rename so the file
// is never briefly visible with the wrong permissions.
//
// mode is explicit rather than inherited: os.CreateTemp happens to produce 0600
// today, but relying on that would leave the permission of a file holding the
// user's model list as an accident of the standard library.
func writeAtomic(path string, payload []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("picker cache: create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".gateway-models-*.tmp")
	if err != nil {
		return fmt.Errorf("picker cache: temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return fmt.Errorf("picker cache: write: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("picker cache: chmod: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("picker cache: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("picker cache: rename into place: %w", err)
	}
	return nil
}

func sameModels(a, b []Model) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
