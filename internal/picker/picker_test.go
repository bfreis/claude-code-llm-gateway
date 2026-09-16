package picker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempCache(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "cache", "gateway-models.json")
}

var sample = []Model{
	{ID: "anthropic/gpt-5.6", DisplayName: "GPT-5.6", Description: "OpenAI"},
}

func TestSyncWritesTheSchemaClaudeCodeValidates(t *testing.T) {
	path := tempCache(t)
	now := time.UnixMilli(1700000000000)

	res, err := Sync(path, "http://127.0.0.1:8787", sample, now)
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if res.State != StateWrote || res.Count != 1 {
		t.Fatalf("Result = %+v", res)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Claude Code parses `{baseUrl, fetchedAt, models:[{id, display_name?,
	// description?}]}`. The JSON keys matter, not the Go field names.
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["baseUrl"] != "http://127.0.0.1:8787" {
		t.Errorf("baseUrl = %v", got["baseUrl"])
	}
	if got["fetchedAt"].(float64) != 1700000000000 {
		t.Errorf("fetchedAt = %v, want epoch milliseconds", got["fetchedAt"])
	}
	models := got["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %v", models)
	}
	m := models[0].(map[string]any)
	if m["id"] != "anthropic/gpt-5.6" || m["display_name"] != "GPT-5.6" || m["description"] != "OpenAI" {
		t.Errorf("model = %v", m)
	}
}

func TestSyncFileIsOwnerOnly(t *testing.T) {
	path := tempCache(t)
	if _, err := Sync(path, "http://x", sample, time.Now()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != CacheFileMode {
		t.Errorf("mode = %o, want %o", perm, CacheFileMode)
	}
}

// TestWriteAtomicAppliesTheRequestedMode pins the mechanism, not just the
// outcome. TestSyncFileIsOwnerOnly alone cannot fail if the chmod is deleted,
// because os.CreateTemp already yields 0600 — so it would pass over a build
// that had stopped setting permissions deliberately. Asking for a mode that is
// NOT the CreateTemp default makes the chmod load-bearing.
func TestWriteAtomicAppliesTheRequestedMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "file.json")
	if err := writeAtomic(path, []byte("{}\n"), 0o640); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %o, want 640 — the requested mode was not applied", perm)
	}
}

func TestSyncIsIdempotent(t *testing.T) {
	path := tempCache(t)
	if _, err := Sync(path, "http://x", sample, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(path, "http://x", sample, time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateUnchanged {
		t.Fatalf("State = %q, want unchanged", res.State)
	}
	// An unchanged sync must not bump fetchedAt, or every serve would rewrite
	// the file for no reason.
	cache, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if cache.FetchedAt != 1 {
		t.Errorf("FetchedAt = %d, want the original timestamp", cache.FetchedAt)
	}
}

func TestSyncRewritesWhenModelsChange(t *testing.T) {
	path := tempCache(t)
	if _, err := Sync(path, "http://x", sample, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(path, "http://x", []Model{{ID: "anthropic/other"}}, time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateWrote {
		t.Errorf("State = %q, want wrote", res.State)
	}
}

func TestSyncRewritesWhenBaseURLChanges(t *testing.T) {
	// Claude Code ignores a cache whose baseUrl differs from its
	// ANTHROPIC_BASE_URL, so a port change has to rewrite the file.
	path := tempCache(t)
	if _, err := Sync(path, "http://127.0.0.1:8787", sample, time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	res, err := Sync(path, "http://127.0.0.1:9999", sample, time.UnixMilli(2))
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateWrote {
		t.Fatalf("State = %q, want wrote", res.State)
	}
	cache, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if cache.BaseURL != "http://127.0.0.1:9999" {
		t.Errorf("BaseURL = %q", cache.BaseURL)
	}
}

func TestSyncEmptyModelsWritesAnArray(t *testing.T) {
	// Claude Code's validator requires models to be an array; `null` fails it
	// and the whole cache is discarded.
	path := tempCache(t)
	if _, err := Sync(path, "http://x", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"models": null`) {
		t.Errorf("models serialised as null:\n%s", raw)
	}
	var cache Cache
	if err := json.Unmarshal(raw, &cache); err != nil {
		t.Fatal(err)
	}
	if cache.Models == nil {
		t.Error("models decoded as nil, want an empty array")
	}
}

func TestSyncCapsAtMaxModels(t *testing.T) {
	path := tempCache(t)
	many := make([]Model, MaxModels+25)
	for i := range many {
		many[i] = Model{ID: "anthropic/m" + string(rune('a'+i%26))}
	}
	res, err := Sync(path, "http://x", many, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != MaxModels {
		t.Errorf("Count = %d, want %d", res.Count, MaxModels)
	}
}

func TestSyncRequiresBaseURL(t *testing.T) {
	if _, err := Sync(tempCache(t), "", sample, time.Now()); err == nil {
		t.Error("Sync with an empty base URL should fail: the reader compares it for equality")
	}
}

func TestSyncLeavesNoTempFiles(t *testing.T) {
	path := tempCache(t)
	if _, err := Sync(path, "http://x", sample, time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "gateway-models.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only the cache file", names)
	}
}

func TestRemove(t *testing.T) {
	path := tempCache(t)
	if _, err := Sync(path, "http://x", sample, time.Now()); err != nil {
		t.Fatal(err)
	}
	res, err := Remove(path)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != StateRemoved {
		t.Errorf("State = %q, want removed", res.State)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("file still present")
	}

	res, err = Remove(path)
	if err != nil {
		t.Fatalf("removing a missing file should not error: %v", err)
	}
	if res.State != StateAbsent {
		t.Errorf("State = %q, want absent", res.State)
	}
}

func TestConfigDirHonoursClaudeConfigDir(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/custom/claude", "cache", "gateway-models.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

func TestPathDefaultsToDotClaude(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	got, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".claude", "cache", "gateway-models.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}
