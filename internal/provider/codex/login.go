package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// LoginResult reports where a completed sign-in was stored.
type LoginResult struct {
	Path      string
	AccountID string
	PlanType  string
}

// Login runs the browser OAuth flow and writes the credential file.
//
// The shape is fixed by the issuer, not chosen here: it only accepts redirect
// URIs from an allow-list, which is why the listener has to bind port 1455
// (or 1457) and why the URI spells the host "localhost" rather than 127.0.0.1.
func Login(ctx context.Context, authPath string, openBrowser bool, out io.Writer) (*LoginResult, error) {
	listener, port, err := listen()
	if err != nil {
		return nil, err
	}
	defer listener.Close()

	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}
	state, err := randomString(32)
	if err != nil {
		return nil, err
	}

	redirect := RedirectURI(port)
	authorize := authorizeURL(redirect, challenge, state)

	fmt.Fprintf(out, "Opening your browser to sign in to ChatGPT.\n")
	fmt.Fprintf(out, "If it does not open, visit:\n\n%s\n\n", authorize)
	if openBrowser {
		_ = browse(authorize)
	}

	code, err := waitForCode(ctx, listener, state)
	if err != nil {
		return nil, err
	}

	tokens, err := exchangeCode(ctx, code, verifier, redirect)
	if err != nil {
		return nil, err
	}
	return persistLogin(authPath, tokens)
}

// listen binds the redirect listener, falling back to the second allow-listed
// port when the first is taken.
func listen() (net.Listener, int, error) {
	var lastErr error
	for _, port := range []int{LoopbackPort, FallbackPort} {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return l, port, nil
		}
		lastErr = err
	}
	return nil, 0, fmt.Errorf(
		"could not bind 127.0.0.1:%d or :%d for the sign-in redirect (is another login in progress, or codex already running?): %w",
		LoopbackPort, FallbackPort, lastErr)
}

func authorizeURL(redirect, challenge, state string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("scope", Scope)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("id_token_add_organizations", "true")
	q.Set("codex_cli_simplified_flow", "true")
	q.Set("state", state)
	q.Set("originator", Originator)
	return AuthorizeURL + "?" + q.Encode()
}

// pkce generates a verifier and its S256 challenge.
//
// The challenge hashes the *encoded* verifier string, per RFC 7636 — hashing
// the raw bytes instead produces a challenge the issuer rejects.
func pkce() (verifier, challenge string, err error) {
	verifier, err = randomString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// waitForCode serves the redirect until the browser comes back with a code.
func waitForCode(ctx context.Context, l net.Listener, state string) (string, error) {
	type outcome struct {
		code string
		err  error
	}
	done := make(chan outcome, 1)

	srv := &http.Server{
		ReadHeaderTimeout: 10 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/auth/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				finishPage(w, "Sign-in failed", e)
				done <- outcome{err: fmt.Errorf("the issuer refused the sign-in: %s", e)}
				return
			}
			// The state check is what stops another page on this machine from
			// feeding us a code it obtained.
			if q.Get("state") != state {
				finishPage(w, "Sign-in failed", "state mismatch")
				done <- outcome{err: errors.New("sign-in state did not match; the redirect did not come from the request this gateway started")}
				return
			}
			code := q.Get("code")
			if code == "" {
				finishPage(w, "Sign-in failed", "no authorization code")
				done <- outcome{err: errors.New("the redirect carried no authorization code")}
				return
			}
			finishPage(w, "Signed in", "You can close this tab and return to the terminal.")
			done <- outcome{code: code}
		}),
	}
	go func() { _ = srv.Serve(l) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case o := <-done:
		return o.code, o.err
	}
}

func finishPage(w http.ResponseWriter, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>%s</title>
<body style="font:16px system-ui;padding:3rem;max-width:34rem;margin:auto">
<h1 style="font-size:1.3rem">%s</h1><p>%s</p></body>`,
		htmlEscape(title), htmlEscape(title), htmlEscape(detail))
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// exchangeCode trades the authorization code for tokens.
//
// Unlike refresh, which is JSON, this leg is form-encoded.
func exchangeCode(ctx context.Context, code, verifier, redirect string) (*TokenResponse, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirect)
	form.Set("client_id", ClientID)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("originator", Originator)
	req.Header.Set("User-Agent", UserAgent(DefaultClientVersion))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange authorization code: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("exchange authorization code: HTTP %d: %s",
			resp.StatusCode, truncate(string(raw), 300))
	}
	var out TokenResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" || out.RefreshToken == "" {
		return nil, errors.New("the token response was missing an access or refresh token")
	}
	return &out, nil
}

// persistLogin writes the credential, merging into an existing file so a
// shared ~/.codex/auth.json keeps whatever else the Codex CLI stored there.
func persistLogin(path string, tr *TokenResponse) (*LoginResult, error) {
	file := &AuthFile{Extra: map[string]json.RawMessage{}}
	if existing, err := LoadAuthFile(path); err == nil {
		file = existing
	}
	file.AuthMode = AuthModeChatGPT
	if file.Tokens == nil {
		file.Tokens = &Tokens{}
	}
	file.Tokens.AccessToken = tr.AccessToken
	file.Tokens.RefreshToken = tr.RefreshToken
	if tr.IDToken != "" {
		file.Tokens.IDToken = tr.IDToken
	}
	if claims, err := AuthClaims(file.Tokens.IDToken); err == nil {
		if id, ok := claims["chatgpt_account_id"].(string); ok {
			file.Tokens.AccountID = id
		}
	}
	file.LastRefresh = time.Now().UTC().Format(time.RFC3339)

	store := &Store{path: path, file: file}
	if err := store.persist(); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return &LoginResult{
		Path:      path,
		AccountID: file.AccountID(),
		PlanType:  file.PlanType(),
	}, nil
}

// browse opens a URL in the user's browser, best effort.
func browse(target string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	c := exec.Command(cmd, append(args, target)...)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	return c.Start()
}
