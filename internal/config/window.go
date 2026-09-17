package config

// Environment variables through which Claude Code learns a model's real input
// context window.
//
// It assumes 200k for any model ID it does not recognise, which is every model
// this gateway advertises. Two variables change that, and which one applies
// depends on whether the advertised ID carries the [1m] suffix:
//
//   - EnvMaxContextTokens sets an arbitrary window, but only for IDs that do
//     not begin with "claude-". Claude Code consults it *after* the suffix
//     check, so a [1m] model never reaches it.
//   - EnvAutoCompactWindow can only lower whatever window resolved. Paired
//     with [1m] — which raises the ceiling to 1M — it expresses any value in
//     its own range; on its own against an unrecognised model it cannot lift
//     the 200k floor.
const (
	EnvMaxContextTokens  = "CLAUDE_CODE_MAX_CONTEXT_TOKENS"
	EnvAutoCompactWindow = "CLAUDE_CODE_AUTO_COMPACT_WINDOW"
)

// The range EnvAutoCompactWindow can express. Claude Code raises a smaller
// value to the minimum and caps a larger one at the maximum.
const (
	MinAutoCompactWindow = 100_000
	MaxAutoCompactWindow = 1_000_000
)

// WindowDirective is the environment variable to export so Claude Code sizes
// its context to the provider models rather than to its 200k guess.
type WindowDirective struct {
	// Name is the environment variable to set.
	Name string
	// Value is what to set it to.
	Value int
	// Mixed reports that the configured models declare different windows.
	// Both variables are process-global, so Value is the smallest of them.
	Mixed bool
	// Largest is the biggest window any model declares. It differs from Value
	// only when Mixed, and names what is being given up.
	Largest int
	// Clamped reports that Value differs from the smallest configured window
	// because the variable cannot express that number.
	Clamped bool
}

// WindowDirective works out how to tell Claude Code the real window.
//
// It returns false when no model declares one, in which case Claude Code keeps
// its 200k assumption and nothing should be exported.
func (c *Config) WindowDirective() (WindowDirective, bool) {
	smallest, largest, longContext, distinct := 0, 0, false, 0
	seen := map[int]bool{}
	for _, m := range c.Models {
		if m.ContextWindow <= 0 {
			continue
		}
		if !seen[m.ContextWindow] {
			seen[m.ContextWindow] = true
			distinct++
		}
		if smallest == 0 || m.ContextWindow < smallest {
			smallest = m.ContextWindow
		}
		if m.ContextWindow > largest {
			largest = m.ContextWindow
		}
		if m.LongContext {
			longContext = true
		}
	}
	if smallest == 0 {
		return WindowDirective{}, false
	}

	d := WindowDirective{Name: EnvMaxContextTokens, Value: smallest, Mixed: distinct > 1, Largest: largest}
	if longContext {
		// The [1m] suffix pins the cap at 1M before EnvMaxContextTokens is
		// consulted, so that variable would be silently inert. Lower from the
		// raised ceiling instead.
		d.Name = EnvAutoCompactWindow
		switch {
		case d.Value < MinAutoCompactWindow:
			d.Value, d.Clamped = MinAutoCompactWindow, true
		case d.Value > MaxAutoCompactWindow:
			d.Value, d.Clamped = MaxAutoCompactWindow, true
		}
	}
	return d, true
}

// EnvToolSearch re-enables Claude Code's on-demand loading of MCP tool schemas.
const EnvToolSearch = "ENABLE_TOOL_SEARCH"

// ToolSearchEnabled reports whether 'ccgw env' should turn tool search on.
//
// It defaults to on. The Anthropic path is a byte-exact proxy, so tool_reference
// blocks cross it untouched, and the translating paths have been exercised
// against a Codex backend. A backend that refuses the shape fails loudly with a
// 400 rather than degrading quietly, and the alternative — every MCP schema
// inlined into every request — costs far more context than anything else the
// gateway does.
func (c *Config) ToolSearchEnabled() bool {
	if c.EnableToolSearch == nil {
		return true
	}
	return *c.EnableToolSearch
}
