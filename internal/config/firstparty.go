package config

// EnvAssumeFirstParty makes Claude Code treat ANTHROPIC_BASE_URL as if it were
// api.anthropic.com.
//
// Claude Code gates a set of features on the base URL being one of Anthropic's
// own hosts. Measured against 2.1.277, the check is one function - call it
// isFirstPartyBaseURL - and it returns true whenever this variable is set,
// before it ever looks at the URL. Everything below is a call site of it:
//
//   - Remote managed settings. Eligibility fails with the internal reason
//     "custom_base_url" behind any other host, and this is the only switch
//     that clears it. The fetch itself goes to a hardcoded
//     api.anthropic.com/api/claude_code/settings with Claude Code's own
//     credential, so it never passes through the gateway - which is why no
//     amount of proxying can substitute for the flag.
//   - The native 1M window on Claude models, instead of the 200k fallback the
//     [1m] suffix otherwise has to work around.
//   - MCP tool search, which is otherwise disabled for being behind a proxy.
//
// And this is what it costs, from the same function:
//
//   - Gateway model discovery is switched off - it requires the base URL *not*
//     to be first-party - and with it the /model picker rows for every
//     provider model, cache or live fetch alike. The picker package writes a
//     curated modelPicker settings file to put them back; EnvCustomModelOption
//     covers the one model reachable without passing --settings.
//
// The leading underscore is Anthropic's, not a convention of this project: it
// is an internal flag, undocumented and free to disappear in any release.
const EnvAssumeFirstParty = "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL"

// EnvGatewayDiscovery makes Claude Code look for a model list at the base URL
// at all - live, and in the cache this gateway writes. EnvAssumeFirstParty
// turns it off, since discovery requires the base URL not to be first-party.
const EnvGatewayDiscovery = "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"

// One extra /model row, for a Claude Code started without the --settings file
// that carries the rest.
//
// Claude Code's model validator accepts EnvCustomModelOption's value verbatim,
// bypassing the catalogue check that otherwise rejects an ID it does not know.
// Where it overlaps the curated rows, Claude Code de-duplicates by model ID.
const (
	EnvCustomModelOption            = "ANTHROPIC_CUSTOM_MODEL_OPTION"
	EnvCustomModelOptionName        = "ANTHROPIC_CUSTOM_MODEL_OPTION_NAME"
	EnvCustomModelOptionDescription = "ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION"
)

// AssumeFirstPartyEnabled reports whether 'ccgw env' should set
// EnvAssumeFirstParty.
//
// It defaults to off. Unlike ToolSearchEnabled, which restores a feature at no
// cost, this one moves the model picker onto a mechanism that needs a
// --settings flag on the claude command line, which no environment variable can
// supply. That is a change to how the user launches Claude Code, so it is
// theirs to opt into.
func (c *Config) AssumeFirstPartyEnabled() bool {
	return c.AssumeFirstParty != nil && *c.AssumeFirstParty
}
