package picker

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Settings is the document Claude Code reads from `--settings`.
//
// Only modelPicker is written. `--settings` is one settings *source* among
// several rather than a replacement for them, so the user's own
// ~/.claude/settings.json and the checkout's settings.local.json still apply;
// the file below adds rows to /model and changes nothing else. Writing
// modelPicker into the user's settings.json would work too, and is what this
// avoids: that file is theirs.
//
// The key is "modelPicker", read as:
//
//	function lne(){ let e=jo(); let n=QX("modelPicker").find(r=>e.includes(r.source));
//	  return n?{picker:n.value,source:n.source}:void 0 }
//
// where jo() is the sources it is honoured from - managed settings, --settings
// and the SDK, and user settings, but never a project checkout. Verified
// against Claude Code 2.1.277 by driving the real picker; see
// scripts/verify-picker.py.
type Settings struct {
	ModelPicker ModelPicker `json:"modelPicker"`
}

// ModelPicker curates the /model rows.
//
// ReplaceBuiltInOptions is deliberately never set: it would hide the Claude
// lineup, and this gateway's whole premise is that Claude models keep working
// alongside the provider ones. Claude Code appends these rows after the
// built-in lineup when it is false or absent.
type ModelPicker struct {
	Options []Option `json:"options"`
}

// Option is one curated row. Model is sent as the model ID verbatim, which is
// what makes an ID Claude Code has never heard of selectable.
type Option struct {
	Model       string `json:"model"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// SettingsPath returns where to write the file, given the gateway's own config
// path: alongside it, rather than in Claude Code's directory.
//
// Unlike the discovery cache this is not a file Claude Code looks for by name -
// it is passed on the command line - so it belongs with ccgw's own state, where
// it is visible and deletable without touching ~/.claude.
func SettingsPath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "model-picker.settings.json")
}

// OptionsFrom renders the advertised catalogue as curated rows.
func OptionsFrom(models []Model) []Option {
	out := make([]Option, 0, len(models))
	for _, m := range models {
		out = append(out, Option{Model: m.ID, Label: m.DisplayName, Description: m.Description})
	}
	return out
}

// SyncSettings writes the --settings document unless it already matches.
func SyncSettings(path string, options []Option) (Result, error) {
	if options == nil {
		options = []Option{}
	}
	if existing, err := ReadSettings(path); err == nil && sameOptions(existing.ModelPicker.Options, options) {
		return Result{State: StateUnchanged, Path: path, Count: len(options)}, nil
	}

	payload, err := json.MarshalIndent(Settings{ModelPicker: ModelPicker{Options: options}}, "", "  ")
	if err != nil {
		return Result{}, fmt.Errorf("picker settings: encode: %w", err)
	}
	payload = append(payload, '\n')

	if err := writeAtomic(path, payload, CacheFileMode); err != nil {
		return Result{}, err
	}
	return Result{State: StateWrote, Path: path, Count: len(options)}, nil
}

// ReadSettings loads an existing settings file.
func ReadSettings(path string) (*Settings, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Settings
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("picker settings: decode %s: %w", path, err)
	}
	return &s, nil
}

// RemoveSettings deletes the settings file.
func RemoveSettings(path string) (Result, error) {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return Result{State: StateAbsent, Path: path}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("picker settings: remove: %w", err)
	}
	return Result{State: StateRemoved, Path: path}, nil
}

func sameOptions(a, b []Option) bool {
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
