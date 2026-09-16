// Package codex talks to a ChatGPT subscription the way the official Codex CLI
// does, so no API key and no second proxy process are involved.
//
// Everything here is modelled on the OAuth flow and credential file of
// openai/codex (Rust, Apache-2.0). Constants are taken from that source rather
// than guessed; each one names where it came from.
package codex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OAuth constants, from codex-rs/login/src/server.rs and
// codex-rs/login/src/auth/manager.rs.
const (
	// Issuer is the OAuth issuer (server.rs DEFAULT_ISSUER).
	Issuer = "https://auth.openai.com"
	// ClientID identifies the Codex CLI to the issuer (manager.rs CLIENT_ID).
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
	// TokenURL exchanges and refreshes tokens.
	TokenURL = Issuer + "/oauth/token"
	// AuthorizeURL begins the browser flow.
	AuthorizeURL = Issuer + "/oauth/authorize"
	// Scope is the scope set the CLI requests.
	Scope = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	// LoopbackPort and FallbackPort are the redirect listener ports. They are
	// not free choices: the issuer's redirect-URI allow-list pins them.
	LoopbackPort = 1455
	FallbackPort = 1457
	// Originator identifies the client to the backend.
	Originator = "codex_cli_rs"
)

// authClaimNamespace is the namespaced claim object in the id_token that
// carries the ChatGPT account id (server.rs jwt_auth_claims).
const authClaimNamespace = "https://api.openai.com/auth"

// RedirectURI is the OAuth redirect for a port.
//
// The host is the literal "localhost" and not 127.0.0.1: the issuer matches the
// redirect URI as a string against its allow-list, so the spelling matters.
func RedirectURI(port int) string {
	return fmt.Sprintf("http://localhost:%d/auth/callback", port)
}

// Tokens is the "tokens" object inside auth.json
// (codex-rs/login/src/token_data.rs TokenData).
type Tokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

// AuthFile is the on-disk credential file
// (codex-rs/login/src/auth/storage.rs AuthDotJson).
//
// Only the fields this gateway needs are modelled. The rest — bedrock keys,
// agent identity, personal access tokens — are preserved on write via Extra so
// that sharing the file with the Codex CLI does not destroy its state.
type AuthFile struct {
	AuthMode     string                     `json:"auth_mode,omitempty"`
	OpenAIAPIKey *string                    `json:"OPENAI_API_KEY"`
	Tokens       *Tokens                    `json:"tokens,omitempty"`
	LastRefresh  string                     `json:"last_refresh,omitempty"`
	Extra        map[string]json.RawMessage `json:"-"`
}

// AuthModeChatGPT is the mode set when signed in with a ChatGPT account.
const AuthModeChatGPT = "chatgpt"

// DefaultHome resolves the Codex home directory: $CODEX_HOME, else ~/.codex.
func DefaultHome() (string, error) {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, ".codex"), nil
}

// DefaultAuthPath is $CODEX_HOME/auth.json, else ~/.codex/auth.json.
func DefaultAuthPath() (string, error) {
	home, err := DefaultHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "auth.json"), nil
}

// LoadAuthFile reads and validates a credential file.
func LoadAuthFile(path string) (*AuthFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Decode twice: once into the known fields, once into a bag so unknown
	// fields survive a later write.
	var f AuthFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, known := range []string{"auth_mode", "OPENAI_API_KEY", "tokens", "last_refresh"} {
		delete(all, known)
	}
	f.Extra = all

	if f.Tokens == nil || f.Tokens.AccessToken == "" {
		return nil, fmt.Errorf("%s has no ChatGPT tokens - run 'codex login', or 'ccgw codex login'", path)
	}
	if f.Tokens.RefreshToken == "" {
		return nil, fmt.Errorf("%s has no refresh token, so the session cannot be renewed - sign in again", path)
	}
	return &f, nil
}

// AccountID returns the ChatGPT account id, preferring the stored value and
// falling back to the claim inside the id_token.
func (f *AuthFile) AccountID() string {
	if f.Tokens == nil {
		return ""
	}
	if f.Tokens.AccountID != "" {
		return f.Tokens.AccountID
	}
	if claims, err := AuthClaims(f.Tokens.IDToken); err == nil {
		if id, ok := claims["chatgpt_account_id"].(string); ok {
			return id
		}
	}
	return ""
}

// PlanType reports the subscription tier named in the id_token, for diagnostics.
func (f *AuthFile) PlanType() string {
	if f.Tokens == nil {
		return ""
	}
	claims, err := AuthClaims(f.Tokens.IDToken)
	if err != nil {
		return ""
	}
	plan, _ := claims["chatgpt_plan_type"].(string)
	return plan
}

// AuthClaims extracts the namespaced auth claim object from a JWT.
func AuthClaims(jwt string) (map[string]any, error) {
	payload, err := JWTPayload(jwt)
	if err != nil {
		return nil, err
	}
	claims, ok := payload[authClaimNamespace].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("token has no %q claim", authClaimNamespace)
	}
	return claims, nil
}

// JWTPayload decodes the claim set of a JWT without verifying its signature.
//
// No verification is needed or possible here: the token is not being trusted as
// proof of anything, it is being read for the account id and expiry that the
// issuer put there and the server will check for itself.
func JWTPayload(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("not a JWT: expected 3 parts, got %d", len(parts))
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode JWT payload: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(decoded, &payload); err != nil {
		return nil, fmt.Errorf("decode JWT claims: %w", err)
	}
	return payload, nil
}

// TokenExpiry reads the `exp` claim of a JWT.
func TokenExpiry(jwt string) (time.Time, error) {
	payload, err := JWTPayload(jwt)
	if err != nil {
		return time.Time{}, err
	}
	exp, ok := payload["exp"].(float64)
	if !ok {
		return time.Time{}, fmt.Errorf("token has no exp claim")
	}
	return time.Unix(int64(exp), 0), nil
}

// refreshLeeway renews a token this far before it actually expires, so a long
// streaming turn does not begin on a credential that dies mid-flight.
const refreshLeeway = 5 * time.Minute

// Store holds the credential in memory and refreshes it when it ages out.
//
// It is safe for concurrent use: one refresh happens at a time and the others
// wait for its result rather than each starting their own.
type Store struct {
	path string

	mu      sync.Mutex
	file    *AuthFile
	expiry  time.Time
	refresh func(refreshToken string) (*TokenResponse, error)
}

// TokenResponse is the issuer's reply to a token or refresh request.
type TokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// NewStore loads the credential file and prepares it for use.
func NewStore(path string) (*Store, error) {
	f, err := LoadAuthFile(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, file: f}
	if exp, err := TokenExpiry(f.Tokens.AccessToken); err == nil {
		s.expiry = exp
	}
	return s, nil
}

// Path reports where the credential was loaded from.
func (s *Store) Path() string { return s.path }

// AccountID returns the ChatGPT account id.
func (s *Store) AccountID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.AccountID()
}

// PlanType returns the subscription tier, for diagnostics.
func (s *Store) PlanType() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.PlanType()
}

// needsRefresh reports whether the access token is at or near expiry. A token
// whose expiry could not be read is assumed good: the server is the authority,
// and a spurious refresh loop would be worse than one 401.
func (s *Store) needsRefresh(now time.Time) bool {
	if s.expiry.IsZero() {
		return false
	}
	return now.Add(refreshLeeway).After(s.expiry)
}

// ExpiryDescription renders the access token's expiry for `codex status`.
func (s *Store) ExpiryDescription() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expiry.IsZero() {
		return "unknown (the token carries no readable expiry)"
	}
	left := time.Until(s.expiry).Round(time.Minute)
	if left <= 0 {
		return fmt.Sprintf("%s (expired; it will be refreshed on the next request)",
			s.expiry.Local().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s (in %s)", s.expiry.Local().Format(time.RFC3339), left)
}
