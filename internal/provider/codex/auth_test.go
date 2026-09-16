package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeJWT builds an unsigned JWT with the given payload. Only the claim set
// matters here: nothing in this package verifies signatures, because the server
// is the one that checks them.
func makeJWT(t *testing.T, payload map[string]any) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(body) + ".sig"
}

func accessToken(t *testing.T, exp time.Time) string {
	t.Helper()
	return makeJWT(t, map[string]any{"exp": float64(exp.Unix())})
}

func idToken(t *testing.T, accountID, plan string) string {
	t.Helper()
	return makeJWT(t, map[string]any{
		authClaimNamespace: map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
		},
	})
}

func writeAuthFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadAuthFileReadsTokens(t *testing.T) {
	path := writeAuthFile(t, `{
	  "auth_mode": "chatgpt",
	  "OPENAI_API_KEY": null,
	  "tokens": {"id_token": "`+idToken(t, "acct_123", "plus")+`",
	             "access_token": "`+accessToken(t, time.Now().Add(time.Hour))+`",
	             "refresh_token": "rt_abc", "account_id": "acct_123"},
	  "last_refresh": "2026-09-16T00:00:00Z"
	}`)

	f, err := LoadAuthFile(path)
	if err != nil {
		t.Fatalf("LoadAuthFile: %v", err)
	}
	if f.Tokens.RefreshToken != "rt_abc" {
		t.Errorf("RefreshToken = %q", f.Tokens.RefreshToken)
	}
	if f.AccountID() != "acct_123" {
		t.Errorf("AccountID() = %q", f.AccountID())
	}
	if f.PlanType() != "plus" {
		t.Errorf("PlanType() = %q", f.PlanType())
	}
}

func TestAccountIDFallsBackToTheIDTokenClaim(t *testing.T) {
	// The CLI may leave account_id null; the claim is the source of truth.
	path := writeAuthFile(t, `{
	  "tokens": {"id_token": "`+idToken(t, "acct_from_claim", "pro")+`",
	             "access_token": "`+accessToken(t, time.Now().Add(time.Hour))+`",
	             "refresh_token": "rt"}
	}`)
	f, err := LoadAuthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.AccountID(); got != "acct_from_claim" {
		t.Errorf("AccountID() = %q, want the claim value", got)
	}
}

func TestLoadAuthFileRejectsUnusableCredentials(t *testing.T) {
	tests := map[string]string{
		"no tokens":        `{"auth_mode":"chatgpt"}`,
		"no access token":  `{"tokens":{"refresh_token":"rt"}}`,
		"no refresh token": `{"tokens":{"access_token":"at"}}`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadAuthFile(writeAuthFile(t, contents)); err == nil {
				t.Error("expected an error naming what to do about it")
			}
		})
	}
}

func TestPersistPreservesFieldsThisGatewayDoesNotModel(t *testing.T) {
	// ~/.codex/auth.json is shared with the Codex CLI. Writing it back must not
	// destroy state we do not understand.
	path := writeAuthFile(t, `{
	  "auth_mode": "chatgpt",
	  "OPENAI_API_KEY": null,
	  "tokens": {"id_token": "`+idToken(t, "a", "plus")+`",
	             "access_token": "`+accessToken(t, time.Now().Add(time.Hour))+`",
	             "refresh_token": "rt"},
	  "last_refresh": "2026-09-16T00:00:00Z",
	  "bedrock_api_key": "secret-value",
	  "agent_identity": {"nested": true}
	}`)

	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.persist(); err != nil {
		t.Fatalf("persist: %v", err)
	}

	var got map[string]any
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["bedrock_api_key"] != "secret-value" {
		t.Errorf("bedrock_api_key was lost: %v", got["bedrock_api_key"])
	}
	if got["agent_identity"] == nil {
		t.Error("agent_identity was lost")
	}
	if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestNeedsRefreshUsesTheAccessTokenExpiry(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		exp  time.Time
		want bool
	}{
		{"fresh", now.Add(time.Hour), false},
		{"inside the 5m leeway", now.Add(2 * time.Minute), true},
		{"already expired", now.Add(-time.Minute), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeAuthFile(t, `{"tokens":{"access_token":"`+accessToken(t, tc.exp)+
				`","refresh_token":"rt","id_token":"`+idToken(t, "a", "plus")+`"}}`)
			s, err := NewStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := s.needsRefresh(now); got != tc.want {
				t.Errorf("needsRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnreadableExpiryIsTreatedAsFresh(t *testing.T) {
	// The server decides; a refresh loop over an expiry we cannot parse would
	// be worse than one 401.
	path := writeAuthFile(t, `{"tokens":{"access_token":"not-a-jwt","refresh_token":"rt"}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.needsRefresh(time.Now()) {
		t.Error("an unparseable expiry should not trigger a refresh")
	}
}

func TestRefreshSendsTheDocumentedBody(t *testing.T) {
	var got struct {
		body        map[string]any
		contentType string
		originator  string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.contentType = r.Header.Get("Content-Type")
		got.originator = r.Header.Get("originator")
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt"}`))
	}))
	defer srv.Close()

	out, err := refreshAt(context.Background(), srv.Client(), srv.URL, "rt_old")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	// Refresh is JSON, unlike the form-encoded authorization-code exchange.
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got.contentType)
	}
	if got.originator != Originator {
		t.Errorf("originator = %q, want %q", got.originator, Originator)
	}
	if got.body["grant_type"] != "refresh_token" || got.body["client_id"] != ClientID ||
		got.body["refresh_token"] != "rt_old" {
		t.Errorf("body = %v", got.body)
	}
	// No scope, redirect_uri or PKCE verifier on a refresh.
	for _, absent := range []string{"scope", "redirect_uri", "code_verifier"} {
		if _, ok := got.body[absent]; ok {
			t.Errorf("body should not carry %q", absent)
		}
	}
	if out.AccessToken == nil || *out.AccessToken != "new-at" {
		t.Errorf("AccessToken = %v", out.AccessToken)
	}
}

func TestRefreshErrorClassification(t *testing.T) {
	tests := []struct {
		name          string
		status        int
		body          string
		wantPermanent bool
	}{
		{"401", http.StatusUnauthorized, `{}`, true},
		{"expired grant", http.StatusBadRequest, `{"error":{"code":"refresh_token_expired"}}`, true},
		{"reused grant", http.StatusBadRequest, `{"error":"refresh_token_reused"}`, true},
		{"invalidated", http.StatusBadRequest, `{"code":"refresh_token_invalidated"}`, true},
		{"invalid_grant", http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		// A server wobble must not tell the user to sign in again.
		{"500", http.StatusInternalServerError, `{}`, false},
		{"429", http.StatusTooManyRequests, `{}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			_, err := refreshAt(context.Background(), srv.Client(), srv.URL, "rt")
			if err == nil {
				t.Fatal("expected an error")
			}
			re, ok := err.(*RefreshError)
			if !ok {
				t.Fatalf("error type = %T, want *RefreshError", err)
			}
			if re.Permanent != tc.wantPermanent {
				t.Errorf("Permanent = %v, want %v (%s)", re.Permanent, tc.wantPermanent, err)
			}
			if tc.wantPermanent && !strings.Contains(err.Error(), "sign in again") {
				t.Errorf("a permanent failure should say what to do: %s", err)
			}
		})
	}
}

func TestAccessTokenKeepsTheOldTokenOnATransientFailure(t *testing.T) {
	old := accessToken(t, time.Now().Add(time.Minute)) // inside the leeway
	path := writeAuthFile(t, `{"tokens":{"access_token":"`+old+`","refresh_token":"rt"}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.refresh = func(string) (*TokenResponse, error) {
		return nil, &RefreshError{Permanent: false, Err: context.DeadlineExceeded}
	}

	got, err := s.AccessToken(context.Background(), nil)
	if err != nil {
		t.Fatalf("a transient refresh failure should not fail the request: %v", err)
	}
	if got != old {
		t.Errorf("token = %q, want the existing one reused", got)
	}
}

func TestAccessTokenFailsOnAPermanentFailure(t *testing.T) {
	path := writeAuthFile(t, `{"tokens":{"access_token":"`+accessToken(t, time.Now())+
		`","refresh_token":"rt"}}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	s.refresh = func(string) (*TokenResponse, error) {
		return nil, &RefreshError{Permanent: true, Code: "refresh_token_expired"}
	}
	if _, err := s.AccessToken(context.Background(), nil); err == nil {
		t.Error("a dead grant must surface, not be papered over")
	}
}

func TestAccessTokenRefreshesAndPersists(t *testing.T) {
	path := writeAuthFile(t, `{"auth_mode":"chatgpt","tokens":{"access_token":"`+
		accessToken(t, time.Now())+`","refresh_token":"rt_old"},"extra_field":"keep me"}`)
	s, err := NewStore(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := accessToken(t, time.Now().Add(2*time.Hour))
	s.refresh = func(rt string) (*TokenResponse, error) {
		if rt != "rt_old" {
			t.Errorf("refresh used %q", rt)
		}
		return &TokenResponse{AccessToken: fresh, RefreshToken: "rt_new"}, nil
	}

	got, err := s.AccessToken(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != fresh {
		t.Error("AccessToken did not return the refreshed token")
	}

	reloaded, err := LoadAuthFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Tokens.RefreshToken != "rt_new" {
		t.Errorf("rotated refresh token was not persisted: %q", reloaded.Tokens.RefreshToken)
	}
	if reloaded.LastRefresh == "" {
		t.Error("last_refresh was not updated")
	}
	if reloaded.Extra["extra_field"] == nil {
		t.Error("unmodelled field was lost on write")
	}
	// A second call must not refresh again now that the token is fresh.
	s.refresh = func(string) (*TokenResponse, error) {
		t.Fatal("refreshed twice for one fresh token")
		return nil, nil
	}
	if _, err := s.AccessToken(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestRedirectURIUsesLocalhostSpelling(t *testing.T) {
	// The issuer matches this string against an allow-list, so 127.0.0.1 in
	// place of "localhost" would be rejected.
	if got := RedirectURI(LoopbackPort); got != "http://localhost:1455/auth/callback" {
		t.Errorf("RedirectURI = %q", got)
	}
}

func TestDefaultAuthPathHonoursCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/codex")
	got, err := DefaultAuthPath()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join("/custom/codex", "auth.json") {
		t.Errorf("DefaultAuthPath = %q", got)
	}
}

func TestUserAgentCarriesTheClientVersion(t *testing.T) {
	// The backend gates model availability on the version it is told, so this
	// must be the configured value and not a constant baked in at build time.
	got := UserAgent("0.154.0")
	if !strings.Contains(got, Originator+"/0.154.0") {
		t.Errorf("UserAgent = %q, want it to carry the version", got)
	}
	if UserAgent("") != UserAgent(DefaultClientVersion) {
		t.Error("an empty version should fall back to the default, not send an empty one")
	}
}

func TestDefaultClientVersionLooksLikeARealRelease(t *testing.T) {
	// A placeholder like 0.1.0 is what earned "requires a newer version of
	// Codex" from the backend; this guards against regressing to one.
	major, minor, ok := strings.Cut(DefaultClientVersion, ".")
	if !ok || major != "0" {
		t.Fatalf("DefaultClientVersion = %q, want a 0.x Codex release", DefaultClientVersion)
	}
	minorNum, _, _ := strings.Cut(minor, ".")
	if len(minorNum) < 3 {
		t.Errorf("DefaultClientVersion = %q: the minor looks like a placeholder, not a Codex release",
			DefaultClientVersion)
	}
}
