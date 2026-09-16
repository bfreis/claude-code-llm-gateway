// Command ccgw is a local LLM gateway for Claude Code.
//
// It speaks the Anthropic Messages API on loopback, proxies Claude models to
// Anthropic byte-for-byte so a Claude subscription keeps working, and
// translates other models to their provider's API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
	"github.com/bfreis/claude-code-llm-gateway/internal/picker"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/codex"
	"github.com/bfreis/claude-code-llm-gateway/internal/router"
	"github.com/bfreis/claude-code-llm-gateway/internal/server"
)

const usage = `ccgw - local LLM gateway for Claude Code

Usage:
  ccgw serve  [-config PATH] [-listen ADDR] [-v]   Run the gateway
  ccgw models [-config PATH]                       Print the advertised catalogue
  ccgw env    [-config PATH] [-mode MODE]          Print the env to launch Claude Code
  ccgw setup  [-y] [-config PATH] [-listen ADDR]    Detect what is installed and write a config
  ccgw init   [-config PATH]                       Write a starter config non-interactively
  ccgw sync-picker [-config PATH] [-remove]        Write Claude Code's model-picker cache
  ccgw codex login  [-auth PATH] [-no-browser]     Sign in to ChatGPT for codex models
  ccgw codex status [-auth PATH]                   Show the stored ChatGPT sign-in

Modes for 'env':
  fidelity  (default) Models appear in /model AND Claude models keep every
                      Claude Code feature. Needs no extra credential; the
                      picker rows come from the cache 'sync-picker' writes.
  discovery           Same picker, but Claude Code fetches /v1/models itself.
                      Needs ANTHROPIC_AUTH_TOKEN, and setting it costs the 1h
                      prompt cache. Use it if you run on an API key.
  gateway             Enterprise gateway mode. Sends the fewest features; kept
                      for completeness only.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "models":
		err = cmdModels(os.Args[2:])
	case "env":
		err = cmdEnv(os.Args[2:])
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "init":
		err = cmdInit(os.Args[2:])
	case "sync-picker":
		err = cmdSyncPicker(os.Args[2:])
	case "codex":
		err = cmdCodex(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "ccgw: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccgw: %v\n", err)
		os.Exit(1)
	}
}

// defaultConfigPath is $XDG_CONFIG_HOME/ccgw/config.yaml, or
// ~/.config/ccgw/config.yaml.
//
// os.UserConfigDir is deliberately not used: on macOS it resolves to
// "~/Library/Application Support", which is neither where anyone looks for a
// CLI tool's config nor pleasant to type, having a space in it.
func defaultConfigPath() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ccgw", "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "ccgw.yaml"
	}
	return filepath.Join(home, ".config", "ccgw", "config.yaml")
}

func newFlagSet(name string) *flag.FlagSet {
	return flag.NewFlagSet(name, flag.ExitOnError)
}

func addConfigFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfigPath(), "path to the gateway config file")
}

func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s - run 'ccgw init' to create one", path)
		}
		return nil, err
	}
	return cfg, nil
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	listen := fs.String("listen", "", "override the configured listen address")
	verbose := fs.Bool("v", false, "log every request at debug level")
	noPicker := fs.Bool("no-picker-sync", false, "do not write Claude Code's model-picker cache")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	for _, w := range router.New(cfg).DiscoveryWarnings() {
		log.Warn(w)
	}
	for _, w := range cfg.CredentialWarnings() {
		log.Warn(w)
	}

	// Build the server before advertising anything. Construction is where a
	// missing credential or an unreachable backend surfaces, and writing the
	// picker cache first would leave Claude Code offering models from a gateway
	// that never started.
	srv, err := server.New(cfg, log)
	if err != nil {
		return err
	}

	if !*noPicker {
		res, err := syncPicker(cfg)
		if err != nil {
			// The gateway is still perfectly usable without the picker rows.
			log.Warn("could not write the model-picker cache", "err", err)
		} else {
			log.Info("model-picker cache "+string(res.State), "path", res.Path, "models", res.Count)
		}
	}

	httpSrv := &http.Server{
		Addr:    cfg.Listen,
		Handler: srv.Handler(),
		// No WriteTimeout: it would cut off long streaming turns.
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Info("gateway listening", "addr", cfg.Listen, "models", len(cfg.Models))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	}
}

func cmdModels(args []string) error {
	fs := flag.NewFlagSet("models", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	r := router.New(cfg)

	// Catalogue() is what the picker and /v1/models advertise, variant suffix
	// included, so listing it here shows exactly what Claude Code will see.
	catalogue := r.Catalogue()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MODEL ID (as Claude Code sees it)\tPROVIDER\tUPSTREAM ID\tDISPLAY NAME")
	for i, m := range cfg.Models {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", catalogue[i].ID, m.Provider, m.ID, catalogue[i].DisplayName)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(cfg.Models) == 0 {
		fmt.Println("(no provider models configured; every model falls through to Anthropic)")
	}
	fmt.Println("\nAny model ID not listed here is proxied to Anthropic unchanged.")
	reportPickerDrift(cfg)
	for _, w := range r.DiscoveryWarnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	for _, w := range cfg.CredentialWarnings() {
		fmt.Fprintf(os.Stderr, "warning: %s\n", w)
	}
	return nil
}

// reportPickerDrift warns when Claude Code's picker cache does not match the
// configured catalogue.
//
// The cache is read once at Claude Code startup and written by the gateway, so
// editing the config without refreshing it leaves the picker showing a stale
// list. That failure is silent and looks like the gateway ignoring the config,
// which is worth one line of output to rule out.
func reportPickerDrift(cfg *config.Config) {
	path, err := picker.Path()
	if err != nil {
		return
	}
	cache, err := picker.Read(path)
	if err != nil {
		if len(cfg.Models) > 0 {
			fmt.Fprintf(os.Stderr,
				"\nnote: no model-picker cache at %s yet - 'ccgw serve' or 'ccgw sync-picker' writes it\n", path)
		}
		return
	}

	want := pickerModels(cfg)
	base := baseURL(cfg)
	switch {
	case cache.BaseURL != base:
		fmt.Fprintf(os.Stderr,
			"\nnote: the picker cache points at %s but this config listens on %s.\n"+
				"      Claude Code ignores a cache whose baseUrl does not match, so the picker\n"+
				"      will show no gateway models. Run 'ccgw sync-picker'.\n", cache.BaseURL, base)
	case !sameModelIDs(cache.Models, want):
		fmt.Fprintf(os.Stderr,
			"\nnote: the picker cache lists %d model(s) but this config has %d.\n"+
				"      Run 'ccgw sync-picker', then restart Claude Code fully.\n",
			len(cache.Models), len(want))
	}
}

func sameModelIDs(a, b []picker.Model) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			return false
		}
	}
	return true
}

func cmdEnv(args []string) error {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	mode := fs.String("mode", "fidelity", "fidelity, discovery or gateway")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	base := "http://" + cfg.Listen
	switch *mode {
	case "discovery":
		fmt.Printf(`# Discovery mode: Claude Code fetches /v1/models from the gateway itself
# instead of reading the cache, so the picker self-updates. Prefer 'fidelity'
# unless you run on an API key - see the cost below.
#
# Claude Code sends ANTHROPIC_AUTH_TOKEN as its credential, so make it a real
# subscription token ('claude setup-token'); with anthropic.auth=passthrough the
# gateway relays it to Anthropic untouched and re-adds the oauth beta that
# Claude Code drops once a token is set this way.
#
# The one feature lost versus 'fidelity': the prompt cache falls back from a 1h
# TTL to the 5-minute default, because Claude Code stops sending ttl in the
# request body once ANTHROPIC_AUTH_TOKEN is set.
export ANTHROPIC_BASE_URL=%s
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
export ANTHROPIC_AUTH_TOKEN="${ANTHROPIC_AUTH_TOKEN:?run: claude setup-token}"
unset ANTHROPIC_API_KEY
`, base)
	case "gateway":
		fmt.Printf(`# Gateway mode: kept for completeness. 'fidelity' does the same job while
# preserving every one of Claude Code's request features - prefer it.
#
# Claude Code lists this gateway's models in /model.
#
# ANTHROPIC_AUTH_TOKEN is the credential Claude Code sends to the gateway. With
# anthropic.auth=passthrough, put a real subscription token here (mint one with
# 'claude setup-token') and the gateway relays it to Anthropic untouched, so
# Claude models keep billing to the subscription.
export CLAUDE_CODE_USE_GATEWAY=1
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
export ANTHROPIC_BASE_URL=%s
export ANTHROPIC_AUTH_TOKEN="${ANTHROPIC_AUTH_TOKEN:?run: claude setup-token}"
unset ANTHROPIC_API_KEY
`, base)
	case "fidelity", "baseurl":
		fmt.Printf(`# Fidelity mode (recommended): nothing is given up. Claude Code sends its own
# credential (subscription OAuth token, or ANTHROPIC_API_KEY if set) straight
# through, with its full beta set and the 1h prompt cache.
#
# The picker rows come from the discovery cache 'ccgw serve' writes, which
# Claude Code reads without needing any extra credential. Re-run
# 'ccgw sync-picker' after changing the model list, then restart Claude Code
# fully - it only re-reads the picker at startup.
export ANTHROPIC_BASE_URL=%s
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
`, base)
	default:
		return fmt.Errorf("unknown mode %q (want fidelity, discovery or gateway)", *mode)
	}
	return nil
}

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	force := fs.Bool("force", false, "overwrite an existing config")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		return fmt.Errorf("%s already exists (pass -force to overwrite)", *cfgPath)
	}
	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*cfgPath, []byte(starterConfig), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", *cfgPath)
	fmt.Println("next: edit it, then run 'ccgw serve' and 'eval \"$(ccgw env)\"'")
	return nil
}

// pickerModels renders the catalogue in the shape Claude Code's cache expects.
func pickerModels(cfg *config.Config) []picker.Model {
	r := router.New(cfg)
	out := make([]picker.Model, 0, len(cfg.Models))
	for _, m := range r.Catalogue() {
		out = append(out, picker.Model{ID: m.ID, DisplayName: m.DisplayName, Description: m.Description})
	}
	return out
}

// baseURL is the ANTHROPIC_BASE_URL this config implies. Claude Code compares
// it to the cache's baseUrl with string equality, so both must be built the
// same way.
func baseURL(cfg *config.Config) string { return "http://" + cfg.Listen }

func syncPicker(cfg *config.Config) (picker.Result, error) {
	path, err := picker.Path()
	if err != nil {
		return picker.Result{}, err
	}
	return picker.Sync(path, baseURL(cfg), pickerModels(cfg), time.Now())
}

func cmdSyncPicker(args []string) error {
	fs := flag.NewFlagSet("sync-picker", flag.ExitOnError)
	cfgPath := addConfigFlag(fs)
	remove := fs.Bool("remove", false, "delete the cache instead of writing it")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *remove {
		path, err := picker.Path()
		if err != nil {
			return err
		}
		res, err := picker.Remove(path)
		if err != nil {
			return err
		}
		fmt.Printf("%s %s\n", res.State, res.Path)
		return nil
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	res, err := syncPicker(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s (%d models, baseUrl %s)\n", res.State, res.Path, res.Count, baseURL(cfg))
	fmt.Println("\nClaude Code reads this at startup. It needs CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1")
	fmt.Println("and an ANTHROPIC_BASE_URL exactly equal to the baseUrl above, and it only")
	fmt.Println("re-reads on a full restart - /reload-plugins does not refresh the picker.")
	return nil
}

// cmdCodex dispatches the codex subcommands.
func cmdCodex(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ccgw codex login|status")
	}
	switch args[0] {
	case "login":
		return cmdCodexLogin(args[1:])
	case "status":
		return cmdCodexStatus(args[1:])
	default:
		return fmt.Errorf("unknown codex command %q (want login or status)", args[0])
	}
}

// codexAuthPath resolves where the ChatGPT credential lives.
func codexAuthPath(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	return codex.DefaultAuthPath()
}

func cmdCodexLogin(args []string) error {
	fs := flag.NewFlagSet("codex login", flag.ExitOnError)
	authPath := fs.String("auth", "", "where to write the credential (default $CODEX_HOME/auth.json)")
	noBrowser := fs.Bool("no-browser", false, "print the URL instead of opening a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := codexAuthPath(*authPath)
	if err != nil {
		return err
	}

	// The browser round trip is the slow part and the user may abandon it.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 5*time.Minute)
	defer timeout()

	res, err := codex.Login(ctx, path, !*noBrowser, os.Stderr)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("sign-in timed out after 5 minutes")
		}
		return err
	}

	fmt.Printf("signed in and wrote %s\n", res.Path)
	if res.PlanType != "" {
		fmt.Printf("  plan:    %s\n", res.PlanType)
	}
	if res.AccountID != "" {
		fmt.Printf("  account: %s\n", res.AccountID)
	}
	fmt.Println("\nAdd a codex provider to your config, then restart Claude Code:")
	fmt.Println("  providers:\n    - name: codex\n      type: codex")
	return nil
}

func cmdCodexStatus(args []string) error {
	fs := flag.NewFlagSet("codex status", flag.ExitOnError)
	authPath := fs.String("auth", "", "credential path (default $CODEX_HOME/auth.json)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := codexAuthPath(*authPath)
	if err != nil {
		return err
	}

	store, err := codex.NewStore(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no ChatGPT credential at %s - run 'ccgw codex login'", path)
		}
		return err
	}
	fmt.Printf("credential: %s\n", store.Path())
	if plan := store.PlanType(); plan != "" {
		fmt.Printf("plan:       %s\n", plan)
	}
	if id := store.AccountID(); id != "" {
		fmt.Printf("account:    %s\n", id)
	}
	fmt.Printf("expires:    %s\n", store.ExpiryDescription())
	return nil
}

const starterConfig = `# ccgw - local LLM gateway for Claude Code
listen: 127.0.0.1:8787

anthropic:
  base_url: https://api.anthropic.com
  # passthrough: relay the credential Claude Code sent. This is what keeps a
  # Claude subscription in play.
  # bearer:      use token_env instead (e.g. a token from 'claude setup-token').
  # api_key:     use api_key_env instead.
  auth: passthrough

  # Extra anthropic-beta values merged into every proxied request.
  #
  # Gateway mode sends fewer betas than base-URL mode, but adding them back
  # here mostly does nothing: those features are gated inside Claude Code on
  # request *body* fields it only emits when its own beta set contains the
  # beta, and that decision is made before the request leaves. A header added
  # downstream cannot bring the field back. Use this only for a beta you know
  # is header-only. oauth-2025-04-20 is added automatically when a
  # subscription token is forwarded.
  add_betas: []

# Claude Code's model discovery drops any ID that does not contain "claude" or
# "anthropic", so non-Anthropic models are advertised under this prefix and the
# gateway strips it again before calling the provider.
alias_prefix: "anthropic/"

providers:
  - name: openai
    type: openai
    base_url: https://api.openai.com/v1
    api_key_env: OPENAI_API_KEY
    # How to translate for this backend. The defaults suit current OpenAI
    # models; change them for an older or third-party endpoint.
    #
    # reasoning: thinking          # thinking (default) | drop
    # max_tokens_field: max_completion_tokens   # or max_tokens for older APIs
    # drop_temperature: false      # true for reasoning models, which reject
    #                              # any temperature but their own default
    # max_tokens_cap: 0            # clamp the output cap; 0 = no clamp
    # keep_plan_tools: false       # EnterPlanMode/ExitPlanMode are withheld by
    #                              # default: they drive Claude Code's own
    #                              # workflow and other models call them
    #                              # unprompted, stalling the turn

  # To spend a ChatGPT Plus/Pro subscription instead of an API key, sign in with
  # 'ccgw codex login' and add a codex provider. It needs no base_url and no
  # api_key: the endpoint is fixed and the credential is OAuth.
  #
  # - name: codex
  #   type: codex
  #   # service_tier: priority
  #
  # type: anthropic-compatible forwards the request unchanged to anything that
  # already speaks the Anthropic Messages API, letting that end translate.
  #
  # - name: elsewhere
  #   type: anthropic-compatible
  #   base_url: http://127.0.0.1:8080

models:
  - id: gpt-5.6
    provider: openai
    display_name: GPT-5.6
    description: OpenAI GPT-5.6
    # long_context: true   # advertise as gpt-5.6[1m], raising Claude Code's
                           # client-side window from its 200k default for an
                           # unrecognised model. The suffix is stripped before
                           # the request reaches the provider, so only set it
                           # if the backend really accepts that much input.

# Every model ID not listed above is proxied to Anthropic unchanged, so all the
# Claude models keep working without being enumerated here.
`
