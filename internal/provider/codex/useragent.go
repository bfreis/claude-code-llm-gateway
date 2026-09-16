package codex

import (
	"fmt"
	"runtime"
)

// DefaultClientVersion is the Codex CLI version this gateway reports.
//
// It is not cosmetic. The backend gates model availability on it and refuses
// newer models outright:
//
//	400 … The 'gpt-5.6-sol' model requires a newer version of Codex.
//	      Please upgrade to the latest app or CLI and try again.
//
// So the value has to track a real Codex release rather than describe this
// gateway. It is sent both as the `version` header and inside the User-Agent,
// matching what the CLI does. When a new model is refused, raise it — ideally
// to whatever `codex --version` reports on the same machine — via the
// provider's client_version setting, which needs no rebuild.
//
// Latest stable release of openai/codex at the time of writing.
const DefaultClientVersion = "0.154.0"

// UserAgent renders the client identifier for a version.
//
// The shape mirrors the Codex CLI's own
// (codex-rs/login/src/auth/default_client.rs get_codex_user_agent):
// "{originator}/{version} ({os}; {arch})".
func UserAgent(version string) string {
	if version == "" {
		version = DefaultClientVersion
	}
	return fmt.Sprintf("%s/%s (%s; %s)", Originator, version, runtime.GOOS, runtime.GOARCH)
}
