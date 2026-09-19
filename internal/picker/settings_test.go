package picker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settingsFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "model-picker.settings.json")
}

func TestSyncSettingsWritesTheKeyClaudeCodeReads(t *testing.T) {
	// The key is modelPicker, and an option's model field is the raw ID. Claude
	// Code ignores an unknown top-level key silently, so a wrong name here is
	// indistinguishable from the feature not existing.
	path := settingsFile(t)
	if _, err := SyncSettings(path, []Option{{Model: "anthropic/a", Label: "A", Description: "the a"}}); err != nil {
		t.Fatalf("SyncSettings: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, raw)
	}
	mp, ok := doc["modelPicker"].(map[string]any)
	if !ok {
		t.Fatalf("no modelPicker key in:\n%s", raw)
	}
	opts, ok := mp["options"].([]any)
	if !ok || len(opts) != 1 {
		t.Fatalf("options is not a 1-element array in:\n%s", raw)
	}
	opt := opts[0].(map[string]any)
	for key, want := range map[string]string{"model": "anthropic/a", "label": "A", "description": "the a"} {
		if got := opt[key]; got != want {
			t.Errorf("option[%q] = %v, want %q", key, got, want)
		}
	}
	// replaceBuiltInOptions would hide the Claude lineup, which this gateway
	// exists to keep working.
	if _, present := mp["replaceBuiltInOptions"]; present {
		t.Errorf("wrote replaceBuiltInOptions, which would hide the Claude models:\n%s", raw)
	}
}

func TestSyncSettingsIsIdempotent(t *testing.T) {
	path := settingsFile(t)
	opts := []Option{{Model: "anthropic/a", Label: "A"}}
	if _, err := SyncSettings(path, opts); err != nil {
		t.Fatal(err)
	}
	res, err := SyncSettings(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateUnchanged {
		t.Errorf("State = %q, want %q", res.State, StateUnchanged)
	}
}

func TestSyncSettingsRewritesWhenTheCatalogueChanges(t *testing.T) {
	path := settingsFile(t)
	if _, err := SyncSettings(path, []Option{{Model: "anthropic/a"}}); err != nil {
		t.Fatal(err)
	}
	res, err := SyncSettings(path, []Option{{Model: "anthropic/a"}, {Model: "anthropic/b"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateWrote {
		t.Errorf("State = %q, want %q", res.State, StateWrote)
	}
	if res.Count != 2 {
		t.Errorf("Count = %d, want 2", res.Count)
	}
}

func TestSyncSettingsEmptyWritesAnArray(t *testing.T) {
	// A null would fail Claude Code's own validation of the block.
	path := settingsFile(t)
	if _, err := SyncSettings(path, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"options": []`; !strings.Contains(string(raw), want) {
		t.Errorf("does not contain %q:\n%s", want, raw)
	}
}

func TestSyncSettingsFileIsOwnerOnly(t *testing.T) {
	// It names the models the user routes, and sits next to their config.
	path := settingsFile(t)
	if _, err := SyncSettings(path, []Option{{Model: "anthropic/a"}}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestRemoveSettings(t *testing.T) {
	path := settingsFile(t)
	res, err := RemoveSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateAbsent {
		t.Errorf("State = %q, want %q", res.State, StateAbsent)
	}
	if _, err := SyncSettings(path, []Option{{Model: "anthropic/a"}}); err != nil {
		t.Fatal(err)
	}
	res, err = RemoveSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateRemoved {
		t.Errorf("State = %q, want %q", res.State, StateRemoved)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still there: %v", err)
	}
}

func TestSettingsPathSitsBesideTheGatewayConfig(t *testing.T) {
	// Not in ~/.claude: Claude Code does not look for this file by name, it is
	// passed on the command line, so it is ccgw's own state.
	got := SettingsPath("/home/u/.config/ccgw/config.yaml")
	if want := "/home/u/.config/ccgw/model-picker.settings.json"; got != want {
		t.Errorf("SettingsPath = %q, want %q", got, want)
	}
}

func TestOptionsFromCarriesLabels(t *testing.T) {
	got := OptionsFrom([]Model{{ID: "anthropic/a", DisplayName: "A", Description: "the a"}})
	want := []Option{{Model: "anthropic/a", Label: "A", Description: "the a"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("OptionsFrom = %+v, want %+v", got, want)
	}
}
