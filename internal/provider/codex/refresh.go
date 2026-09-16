package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// RefreshError classifies a failed token refresh.
//
// The distinction matters: a transient failure should be retried on the next
// request, while a permanent one means the stored grant is dead and no amount
// of retrying will help — the user has to sign in again.
type RefreshError struct {
	Permanent bool
	Code      string
	Status    int
	Err       error
}

func (e *RefreshError) Error() string {
	kind := "transient"
	if e.Permanent {
		kind = "permanent"
	}
	msg := fmt.Sprintf("codex token refresh failed (%s", kind)
	if e.Status != 0 {
		msg += fmt.Sprintf(", HTTP %d", e.Status)
	}
	if e.Code != "" {
		msg += ", " + e.Code
	}
	msg += ")"
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	if e.Permanent {
		msg += " - sign in again with 'ccgw codex login'"
	}
	return msg
}

func (e *RefreshError) Unwrap() error { return e.Err }

// permanentRefreshCodes are the issuer error codes that mean the grant is gone
// for good (codex-rs/login/src/auth/manager.rs).
var permanentRefreshCodes = map[string]bool{
	"refresh_token_expired":     true,
	"refresh_token_reused":      true,
	"refresh_token_invalidated": true,
	"invalid_grant":             true,
}

// refreshRequest is the body the issuer expects.
//
// Note the asymmetry with the authorization-code exchange, which is
// form-encoded: refresh is JSON, and carries no scope, redirect_uri or PKCE
// verifier (manager.rs RefreshRequest).
type refreshRequest struct {
	ClientID     string `json:"client_id"`
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
}

// refreshResponse is the issuer's reply. Every field is optional: the issuer
// returns only what changed, so a missing refresh_token means "keep the one you
// have", not "you no longer have one".
type refreshResponse struct {
	IDToken      *string `json:"id_token"`
	AccessToken  *string `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

// Refresh exchanges a refresh token for a fresh access token.
func Refresh(ctx context.Context, client *http.Client, refreshToken string) (*refreshResponse, error) {
	return refreshAt(ctx, client, TokenURL, refreshToken)
}

// refreshAt is Refresh against a caller-supplied endpoint, so the flow can be
// exercised against a test server.
func refreshAt(ctx context.Context, client *http.Client, tokenURL, refreshToken string) (*refreshResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}
	body, err := json.Marshal(refreshRequest{
		ClientID:     ClientID,
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	})
	if err != nil {
		return nil, &RefreshError{Err: err}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, &RefreshError{Err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent(DefaultClientVersion))

	resp, err := client.Do(req)
	if err != nil {
		// A network failure says nothing about the grant's validity.
		return nil, &RefreshError{Err: err}
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code := issuerErrorCode(raw)
		permanent := resp.StatusCode == http.StatusUnauthorized ||
			permanentRefreshCodes[code] ||
			(resp.StatusCode == http.StatusBadRequest && code == "invalid_grant")
		return nil, &RefreshError{
			Permanent: permanent,
			Code:      code,
			Status:    resp.StatusCode,
			Err:       errors.New(truncate(string(raw), 300)),
		}
	}

	var out refreshResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &RefreshError{Status: resp.StatusCode, Err: fmt.Errorf("decode refresh response: %w", err)}
	}
	if out.AccessToken == nil || *out.AccessToken == "" {
		return nil, &RefreshError{Status: resp.StatusCode, Err: errors.New("refresh response carried no access_token")}
	}
	return &out, nil
}

// issuerErrorCode digs the error code out of the several shapes the issuer uses:
// {"error":{"code":...}}, {"error":"..."} or {"code":...}.
func issuerErrorCode(raw []byte) string {
	var withObject struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &withObject); err == nil {
		if withObject.Error.Code != "" {
			return withObject.Error.Code
		}
		if withObject.Code != "" {
			return withObject.Code
		}
	}
	var withString struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &withString); err == nil && withString.Error != "" {
		return withString.Error
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// AccessToken returns a usable access token, refreshing it first when it is at
// or near expiry.
//
// Concurrent callers share one refresh: the mutex is held across the HTTP call
// so that a burst of requests at expiry produces a single round trip rather
// than one per request, which the issuer would treat as token reuse.
func (s *Store) AccessToken(ctx context.Context, client *http.Client) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.needsRefresh(time.Now()) {
		return s.file.Tokens.AccessToken, nil
	}

	refresh := s.refresh
	if refresh == nil {
		refresh = func(rt string) (*TokenResponse, error) {
			out, err := Refresh(ctx, client, rt)
			if err != nil {
				return nil, err
			}
			tr := &TokenResponse{}
			if out.IDToken != nil {
				tr.IDToken = *out.IDToken
			}
			if out.AccessToken != nil {
				tr.AccessToken = *out.AccessToken
			}
			if out.RefreshToken != nil {
				tr.RefreshToken = *out.RefreshToken
			}
			return tr, nil
		}
	}

	tr, err := refresh(s.file.Tokens.RefreshToken)
	if err != nil {
		var re *RefreshError
		if errors.As(err, &re) && !re.Permanent {
			// Transient: the token we hold may still have life in it. Let the
			// request proceed and try again next time rather than failing here.
			return s.file.Tokens.AccessToken, nil
		}
		return "", err
	}

	s.applyTokens(tr)
	if err := s.persist(); err != nil {
		// A credential that cannot be written back is still usable in memory;
		// the cost is re-refreshing after a restart, not a broken session.
		return s.file.Tokens.AccessToken, nil
	}
	return s.file.Tokens.AccessToken, nil
}

// applyTokens writes back only the fields the issuer actually returned.
func (s *Store) applyTokens(tr *TokenResponse) {
	if tr.AccessToken != "" {
		s.file.Tokens.AccessToken = tr.AccessToken
	}
	if tr.IDToken != "" {
		s.file.Tokens.IDToken = tr.IDToken
	}
	if tr.RefreshToken != "" {
		s.file.Tokens.RefreshToken = tr.RefreshToken
	}
	s.file.LastRefresh = time.Now().UTC().Format(time.RFC3339)

	s.expiry = time.Time{}
	if exp, err := TokenExpiry(s.file.Tokens.AccessToken); err == nil {
		s.expiry = exp
	}
}

// persist writes the credential file back, preserving fields this gateway does
// not model so that a shared ~/.codex/auth.json keeps working for the Codex CLI.
func (s *Store) persist() error {
	merged := map[string]json.RawMessage{}
	for k, v := range s.file.Extra {
		merged[k] = v
	}
	set := func(key string, value any) error {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		merged[key] = encoded
		return nil
	}
	if s.file.AuthMode != "" {
		if err := set("auth_mode", s.file.AuthMode); err != nil {
			return err
		}
	}
	if err := set("OPENAI_API_KEY", s.file.OpenAIAPIKey); err != nil {
		return err
	}
	if err := set("tokens", s.file.Tokens); err != nil {
		return err
	}
	if err := set("last_refresh", s.file.LastRefresh); err != nil {
		return err
	}

	payload, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeAtomic(s.path, payload, 0o600)
}

// writeAtomic replaces a file in one step, so a concurrent reader — the Codex
// CLI, or another ccgw — never sees a partial credential.
func writeAtomic(path string, payload []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".auth-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
