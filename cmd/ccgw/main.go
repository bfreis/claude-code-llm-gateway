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
	"strings"
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

Set assume_first_party: true in the config to get remote managed settings and
the native 1M Claude window back, at the cost of the /model rows. It applies to
'fidelity' only - the other two modes depend on the discovery it switches off.
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
		res, err := syncPicker(cfg, *cfgPath)
		if err != nil {
			// The gateway is still perfectly usable without the picker rows.
			log.Warn("could not write the model picker", "err", err)
		} else if cfg.AssumeFirstPartyEnabled() {
			log.Info("model-picker settings "+string(res.State),
				"path", res.Path, "models", res.Count, "pass_with", "claude --settings "+res.Path)
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
	fmt.Fprintln(tw, "MODEL ID (as Claude Code sees it)\tPROVIDER\tUPSTREAM ID\tWINDOW\tDISPLAY NAME")
	for i, m := range cfg.Models {
		window := "200k (assumed)"
		if m.ContextWindow > 0 {
			window = fmt.Sprintf("%d", m.ContextWindow)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			catalogue[i].ID, m.Provider, m.ID, window, catalogue[i].DisplayName)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(cfg.Models) == 0 {
		fmt.Println("(no provider models configured; every model falls through to Anthropic)")
	}
	fmt.Println("\nAny model ID not listed here is proxied to Anthropic unchanged.")
	if d, ok := cfg.WindowDirective(); ok {
		fmt.Printf("'ccgw env' exports %s=%d so Claude Code sizes the context to that.\n", d.Name, d.Value)
	} else if len(cfg.Models) > 0 {
		fmt.Println("No model states a context_window, so Claude Code will assume 200k for all of them.")
	}
	reportPickerDrift(cfg, *cfgPath)
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
func reportPickerDrift(cfg *config.Config, cfgPath string) {
	// With assume_first_party the rows come from a curated settings file, so
	// the cache below is not what drifts - that file is.
	if cfg.AssumeFirstPartyEnabled() {
		path := picker.SettingsPath(cfgPath)
		want := picker.OptionsFrom(pickerModels(cfg))
		got, err := picker.ReadSettings(path)
		switch {
		case err != nil:
			fmt.Fprintf(os.Stderr,
				"\nnote: no model-picker settings at %s yet - 'ccgw serve' or\n"+
					"      'ccgw sync-picker' writes it. Until then these models have no /model row.\n", path)
		case len(got.ModelPicker.Options) != len(want):
			fmt.Fprintf(os.Stderr,
				"\nnote: the model-picker settings list %d model(s) but this config has %d.\n"+
					"      Run 'ccgw sync-picker', then restart Claude Code fully.\n",
				len(got.ModelPicker.Options), len(want))
		}
		return
	}
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

	// The flag and these two modes are mutually exclusive by construction, not
	// by preference. 'discovery' exists to have Claude Code fetch /v1/models,
	// which the flag switches off; 'gateway' takes the provider out of
	// first-party entirely, so the flag is never consulted and managed settings
	// fail on gateway pinning instead. Either combination reads as the flag
	// silently doing nothing.
	if cfg.AssumeFirstPartyEnabled() && *mode != "fidelity" && *mode != "baseurl" {
		return fmt.Errorf("assume_first_party is set in the config, which turns gateway model "+
			"discovery off - %s mode depends on it. Use -mode fidelity, or set assume_first_party: false", *mode)
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
		if cfg.AssumeFirstPartyEnabled() {
			printFirstPartyMode(base)
			break
		}
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
	printWindowDirective(cfg)
	printPickerRows(cfg, *cfgPath)
	printToolSearch(cfg)
	printClaudeWindowNote(cfg)
	return nil
}

// printFirstPartyMode emits fidelity mode with EnvAssumeFirstParty set.
//
// It is a block of its own rather than a line added to the fidelity one
// because the two describe opposite arrangements: fidelity mode's picker rows
// come from the discovery cache, and this flag is precisely what stops Claude
// Code reading it.
func printFirstPartyMode(base string) {
	fmt.Printf(`# Fidelity mode with assume_first_party: Claude Code is told to treat this
# gateway's base URL as if it were api.anthropic.com. Everything else about
# fidelity mode is unchanged - Claude Code still sends its own credential, its
# full beta set and the 1h prompt cache.
#
# What the flag restores:
#   - Remote managed settings. Behind a custom base URL Claude Code declines to
#     fetch them at all - its internal reason is "custom_base_url" - and this is
#     the only switch that clears it. The fetch goes straight to
#     api.anthropic.com with your own login and never passes through the
#     gateway, which is why no amount of proxying substitutes for it. A managed
#     settings file installed by MDM applies either way; this is the half that
#     arrives over the network.
#   - The native 1M window on Claude models, so no [1m] suffix is needed.
#   - MCP tool search, otherwise disabled purely for being behind a proxy.
#
# What it costs: the same switch turns gateway model discovery off, so
# %s becomes inert - cleared below rather
# than left looking effective - and the provider models lose the /model rows it
# fed them, from the live fetch and from the cache alike. They come back from a
# curated settings file instead; see the alias further down.
#
# The leading underscore is Anthropic's, not this project's: it is an internal
# flag, undocumented and free to disappear in any release. If managed settings
# stop arriving after an upgrade, check this first.
export ANTHROPIC_BASE_URL=%s
export %s=1
unset %s
`, config.EnvGatewayDiscovery,
		base, config.EnvAssumeFirstParty, config.EnvGatewayDiscovery)
}

// printPickerRows restores the /model rows that EnvAssumeFirstParty costs.
//
// Two mechanisms, because they fail differently. The curated settings file
// carries every model with its own label, but only for a Claude Code invoked
// with --settings, which is what the alias is for. EnvCustomModelOption
// carries one model and needs no flag, so a bare `claude` in this shell is not
// left with nothing. Claude Code de-duplicates the overlap by model ID, and
// the values agree because both come from the same config entry.
func printPickerRows(cfg *config.Config, cfgPath string) {
	if !cfg.AssumeFirstPartyEnabled() {
		return
	}
	models := pickerModels(cfg)
	fmt.Println()
	if len(models) == 0 {
		fmt.Printf("# No provider models are configured, so there are no rows to restore and\n")
		fmt.Printf("# assume_first_party costs nothing. Cleared in case the shell had them set.\n")
		fmt.Printf("unset %s %s %s\n",
			config.EnvCustomModelOption, config.EnvCustomModelOptionName, config.EnvCustomModelOptionDescription)
		return
	}

	settings := picker.SettingsPath(cfgPath)
	fmt.Printf("# Gateway discovery is off, so the %d provider model(s) reach /model as curated\n", len(models))
	fmt.Printf("# rows in a settings file instead. 'ccgw serve' and 'ccgw sync-picker' write it;\n")
	fmt.Printf("# Claude Code only reads it when passed --settings, hence the alias. It is an\n")
	fmt.Printf("# additional settings source, not a replacement, so your own settings.json and\n")
	fmt.Printf("# the checkout's settings.local.json still apply. Drop the alias if you pass\n")
	fmt.Printf("# --settings yourself, and merge modelPicker into that file instead.\n")
	fmt.Printf("alias claude=%s\n", shellQuote("claude --settings "+shellQuote(settings)))

	m := models[0]
	fmt.Printf("#\n")
	fmt.Printf("# And the same first model again, for a 'claude' that bypasses the alias -\n")
	fmt.Printf("# 'command claude', a script, an editor extension. This variable needs no flag\n")
	fmt.Printf("# but holds only one model, and Claude Code accepts its value verbatim rather\n")
	fmt.Printf("# than checking it against a catalogue.\n")
	fmt.Printf("export %s=%s\n", config.EnvCustomModelOption, shellQuote(m.ID))
	if m.DisplayName != "" {
		fmt.Printf("export %s=%s\n", config.EnvCustomModelOptionName, shellQuote(m.DisplayName))
	}
	if m.Description != "" {
		fmt.Printf("export %s=%s\n", config.EnvCustomModelOptionDescription, shellQuote(m.Description))
	}
}

// shellQuote renders s as a single-quoted shell word. This output is eval'd,
// and display names and descriptions come from the config, so they cannot be
// interpolated raw.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printToolSearch re-enables on-demand loading of MCP tool schemas.
//
// Claude Code disables tool search behind any base URL that is not one of
// Anthropic's own, because it cannot know whether the proxy forwards
// tool_reference blocks, and falls back to inlining every MCP schema into every
// request. In a session with a few MCP servers that is the single largest thing
// the gateway costs you.
func printToolSearch(cfg *config.Config) {
	fmt.Println()
	if cfg.ToolSearchEnabled() && cfg.AssumeFirstPartyEnabled() {
		// Redundant against the flag, which already stops Claude Code
		// disabling tool search - but the flag is internal and may go away,
		// and this variable is the documented lever. Keeping both means losing
		// the flag costs managed settings, not 191k of inlined schemas too.
		fmt.Printf("# Load MCP tool schemas on demand. assume_first_party alone is enough for\n")
		fmt.Printf("# this - Claude Code only disables tool search behind a base URL it thinks\n")
		fmt.Printf("# is foreign - but this is the documented lever and the flag is not, so it\n")
		fmt.Printf("# is set explicitly. Managed settings can still override it; exporting\n")
		fmt.Printf("# %s=force after the eval wins.\n", config.EnvToolSearch)
		fmt.Printf("export %s=true\n", config.EnvToolSearch)
		return
	}
	if !cfg.ToolSearchEnabled() {
		fmt.Printf("# enable_tool_search is off in the config, so MCP tool schemas are inlined\n")
		fmt.Printf("# into every request. Cleared here in case the shell already had it set.\n")
		fmt.Printf("unset %s\n", config.EnvToolSearch)
		return
	}
	fmt.Printf("# Load MCP tool schemas on demand instead of inlining every one of them.\n")
	fmt.Printf("# Claude Code switches this off behind a non-first-party base URL because it\n")
	fmt.Printf("# cannot know whether the proxy forwards tool_reference blocks; this one does,\n")
	fmt.Printf("# byte for byte on the Anthropic path. Set enable_tool_search: false in the\n")
	fmt.Printf("# config if a backend answers 400 to the request shape it produces.\n")
	fmt.Printf("export %s=true\n", config.EnvToolSearch)
}

// printClaudeWindowNote explains why Claude models shrink behind the gateway.
//
// Claude Code downgrades a model whose catalogue entry declares a 1M window to
// the 200k it "believes" whenever ANTHROPIC_BASE_URL is not one of Anthropic's
// own hosts, which is always true here. That looks exactly like the gateway
// breaking Claude models, and the fix is not discoverable, so say both.
//
// Everything printed is a comment: this output is eval'd.
func printClaudeWindowNote(cfg *config.Config) {
	if cfg.AssumeFirstPartyEnabled() {
		fmt.Print(`
# Claude models keep their native 1M window here. The 200k fallback applies
# only behind a base URL Claude Code considers foreign, and the flag above makes
# it consider this one its own, so /model sonnet reads as 1.0M and the [1m]
# suffix is not needed. It still works if you prefer to be explicit.
`)
		return
	}
	fmt.Print(`
# Claude models: pointing ANTHROPIC_BASE_URL at anything other than Anthropic's
# own host costs them their native 1M window. Claude Code keeps the catalogue's
# declared 1000000 but falls back to the 200000 it believes, so Sonnet 5 reads
# as 200.0k here and 1.0M without the gateway.
#
# Selecting the 1M variant restores it - that suffix is checked before the
# fallback, so it holds behind any base URL:
#
#     /model sonnet[1m]        (or opus[1m], fable[1m], opusplan[1m])
#
# The choice persists, and the alias always resolves to the current model, so
# there is nothing to keep up to date as new models ship. This affects Claude
# models only; provider models are sized by their config context_window.
`)
}

// printWindowDirective exports the real context window of the provider models.
//
// Claude Code assumes 200k for any ID it does not recognise, which is every ID
// this gateway advertises, so without this a session compacts long before the
// provider would have refused anything.
func printWindowDirective(cfg *config.Config) {
	d, ok := cfg.WindowDirective()
	if !ok {
		return
	}
	fmt.Println()
	fmt.Printf("# Claude Code assumes a 200k window for a model it does not recognise.\n")
	switch d.Name {
	case config.EnvMaxContextTokens:
		fmt.Printf("# This is the window your provider models really have. It applies only to\n")
		fmt.Printf("# model IDs that do not begin with \"claude-\", so the Claude models proxied\n")
		fmt.Printf("# through keep their own windows untouched.\n")
	case config.EnvAutoCompactWindow:
		fmt.Printf("# A model configured with long_context is advertised as [1m], which pins\n")
		fmt.Printf("# Claude Code's cap at 1M before it reads %s.\n", config.EnvMaxContextTokens)
		fmt.Printf("# This variable lowers it from there to the real number - but it is global,\n")
		fmt.Printf("# so it also caps any 1M Claude session at the same value.\n")
	}
	if d.Mixed {
		fmt.Printf("# Your models declare different windows and this variable is process-wide,\n")
		fmt.Printf("# so it takes the smallest. The largest is %d, and that model is held\n", d.Largest)
		fmt.Printf("# to %d here. Run separate sessions if that matters.\n", d.Value)
	}
	if d.Clamped {
		fmt.Printf("# Clamped: %s only expresses %d-%d.\n",
			d.Name, config.MinAutoCompactWindow, config.MaxAutoCompactWindow)
	}
	fmt.Printf("export %s=%d\n", d.Name, d.Value)
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

// syncPicker makes the /model picker show the configured models, by whichever
// mechanism this arrangement leaves available.
//
// With assume_first_party set, Claude Code does not read the discovery cache at
// all - the flag turns discovery off - so the rows have to come from a
// modelPicker block in a --settings file instead. Writing the cache anyway
// would report success for a file nothing reads.
func syncPicker(cfg *config.Config, cfgPath string) (picker.Result, error) {
	if cfg.AssumeFirstPartyEnabled() {
		return picker.SyncSettings(picker.SettingsPath(cfgPath), picker.OptionsFrom(pickerModels(cfg)))
	}
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

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	if *remove {
		// Remove whichever file this arrangement wrote, and the other one too:
		// the usual reason to run this is that the gateway is going away, and
		// a config edited since the last sync should not leave one behind.
		var results []picker.Result
		if res, err := picker.RemoveSettings(picker.SettingsPath(*cfgPath)); err != nil {
			return err
		} else if res.State != picker.StateAbsent {
			results = append(results, res)
		}
		if path, err := picker.Path(); err == nil {
			if res, err := picker.Remove(path); err != nil {
				return err
			} else if res.State != picker.StateAbsent {
				results = append(results, res)
			}
		}
		if len(results) == 0 {
			fmt.Println("absent (nothing to remove)")
			return nil
		}
		for _, res := range results {
			fmt.Printf("%s %s\n", res.State, res.Path)
		}
		return nil
	}

	res, err := syncPicker(cfg, *cfgPath)
	if err != nil {
		return err
	}
	if cfg.AssumeFirstPartyEnabled() {
		fmt.Printf("%s %s (%d models)\n", res.State, res.Path, res.Count)
		fmt.Printf("\nassume_first_party is on, so these rows reach /model as curated settings\n")
		fmt.Printf("rather than through gateway discovery, which the flag turns off. Claude Code\n")
		fmt.Printf("has to be told about the file:\n\n")
		fmt.Printf("    claude --settings %s\n\n", res.Path)
		fmt.Printf("'ccgw env' emits an alias that does that. It is read at startup only, so\n")
		fmt.Printf("restart Claude Code fully after changing the model list.\n")
		return nil
	}
	fmt.Printf("%s %s (%d models, baseUrl %s)\n", res.State, res.Path, res.Count, baseURL(cfg))
	if cfg.AssumeFirstPartyEnabled() {
		fmt.Printf("\nassume_first_party is on in the config, which turns gateway model discovery\n")
		fmt.Printf("off - so Claude Code will not read this cache. The file is written anyway, to\n")
		fmt.Printf("be there if you set assume_first_party: false again.\n")
		return nil
	}
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

# Ask Claude Code to treat this gateway's base URL as api.anthropic.com's own.
#
# On: remote managed settings are fetched again - behind a custom base URL
# Claude Code refuses to, so an org that delivers settings over the network
# stops reaching you - Claude models keep their native 1M window, and MCP tool
# search is not disabled for being behind a proxy.
#
# The cost is not the models - 'ccgw sync-picker' writes them to a curated
# settings file instead, and 'ccgw env' emits the alias that passes it to
# Claude Code - it is that launching plain 'claude' no longer shows all of
# them, because gateway discovery is what this switch turns off. Only
# 'ccgw env -mode fidelity' accepts it.
#
# It is an internal, undocumented Claude Code flag and may stop working.
assume_first_party: false

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
    # context_window: 400000
                           # the model's real input limit. Claude Code assumes
                           # 200k for an ID it does not recognise, so without
                           # this it compacts early; 'ccgw env' exports the
                           # variable that corrects it. Measure rather than
                           # guess - overstating it turns a graceful compact
                           # into a hard refusal from the provider.
    # long_context: true   # advertise as gpt-5.6[1m]. Only for a backend that
                           # really accepts 1M; it also changes which variable
                           # 'ccgw env' uses. The suffix is stripped before the
                           # request reaches the provider.

# Every model ID not listed above is proxied to Anthropic unchanged, so all the
# Claude models keep working without being enumerated here.
`
