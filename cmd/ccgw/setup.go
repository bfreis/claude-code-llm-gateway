package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
	"github.com/bfreis/claude-code-llm-gateway/internal/provider/codex"
)

// environment is everything setup can work out without asking.
//
// The aim is that a machine where both CLIs already work needs no answers at
// all: whatever can be read from an installed tool is read rather than asked.
type environment struct {
	claudeInstalled bool
	claudeLoggedIn  bool
	claudeAuthKind  string // "claude.ai" for a subscription
	claudeKeySource string // set when an API key overrides the subscription
	claudeConfigDir string

	codexInstalled bool
	codexVersion   string
	codexSignedIn  bool
	codexAuthPath  string
	codexPlan      string
	codexModel     string // from the Codex CLI's own config.toml

	openAIKeyEnv string
	portFree     bool
	listen       string
}

func detect(listen string) environment {
	e := environment{listen: listen, portFree: portAvailable(listen)}
	e.detectClaude()
	e.detectCodex()
	for _, name := range []string{"OPENAI_API_KEY", "OPENAI_KEY"} {
		if os.Getenv(name) != "" {
			e.openAIKeyEnv = name
			break
		}
	}
	return e
}

// detectClaude asks Claude Code about its own sign-in.
//
// `claude auth status --json` reports whether the session is a claude.ai
// subscription and whether an API key is overriding it, which is the one
// setting that quietly defeats the point of this gateway.
func (e *environment) detectClaude() {
	if path, err := exec.LookPath("claude"); err != nil || path == "" {
		return
	}
	e.claudeInstalled = true

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "claude", "auth", "status", "--json").Output()
	if err != nil {
		return
	}
	var status struct {
		LoggedIn        bool   `json:"loggedIn"`
		AuthMethod      string `json:"authMethod"`
		APIKeySource    string `json:"apiKeySource"`
		ConfigDirectory string `json:"configDirectory"`
	}
	if err := json.Unmarshal(out, &status); err != nil {
		return
	}
	e.claudeLoggedIn = status.LoggedIn
	e.claudeAuthKind = status.AuthMethod
	e.claudeKeySource = status.APIKeySource
	e.claudeConfigDir = status.ConfigDirectory
}

func (e *environment) detectCodex() {
	if path, err := exec.LookPath("codex"); err == nil && path != "" {
		e.codexInstalled = true
		e.codexVersion = codexCLIVersion()
	}
	if p, err := codex.DefaultAuthPath(); err == nil {
		e.codexAuthPath = p
		if store, err := codex.NewStore(p); err == nil {
			e.codexSignedIn = true
			e.codexPlan = store.PlanType()
		}
	}
	e.codexModel = codexConfiguredModel()
}

var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:-[0-9A-Za-z.\-]+)?`)

// codexCLIVersion asks the installed Codex CLI what version it is.
//
// The backend gates model availability on this value, so matching the installed
// CLI is the difference between a model working and being told it "requires a
// newer version of Codex".
func codexCLIVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "codex", "--version").Output()
	if err != nil {
		return ""
	}
	return versionPattern.FindString(string(out))
}

// codexModelPattern finds a top-level `model = "..."` assignment.
var codexModelPattern = regexp.MustCompile(`(?m)^\s*model\s*=\s*["']([^"']+)["']`)

// codexConfiguredModel reads the model the Codex CLI is set to use, from
// $CODEX_HOME/config.toml.
//
// This is a deliberately shallow scan rather than a TOML parse: all that is
// wanted is one known-good model ID for this account, and a value under some
// other table would still be a real model. A wrong guess costs nothing because
// the answer is shown for confirmation before anything is written.
func codexConfiguredModel() string {
	home, err := codex.DefaultHome()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		return ""
	}
	if m := codexModelPattern.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return ""
}

func portAvailable(listen string) bool {
	l, err := net.Listen("tcp", listen)
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}

// report prints what was detected, so the user can see why setup is or is not
// about to ask them something.
func (e environment) report() {
	line := func(label, value string) { fmt.Printf("  %-16s %s\n", label, value) }

	switch {
	case !e.claudeInstalled:
		line("Claude Code", "not found on PATH")
	case e.claudeLoggedIn && e.claudeAuthKind == "claude.ai":
		line("Claude Code", "signed in with a claude.ai subscription")
	case e.claudeLoggedIn:
		line("Claude Code", "signed in ("+e.claudeAuthKind+")")
	default:
		line("Claude Code", "installed but not signed in - run 'claude login'")
	}
	if e.claudeKeySource != "" {
		line("", "warning: "+e.claudeKeySource+" is set and takes precedence over the")
		line("", "subscription; unset it before launching Claude Code")
	}

	switch {
	case e.codexSignedIn && e.codexPlan != "":
		line("ChatGPT (Codex)", "signed in ("+e.codexPlan+" plan)")
	case e.codexSignedIn:
		line("ChatGPT (Codex)", "signed in")
	case e.codexInstalled:
		line("ChatGPT (Codex)", "CLI installed, not signed in")
	default:
		line("ChatGPT (Codex)", "no sign-in found")
	}
	if e.codexVersion != "" {
		line("", "CLI version "+e.codexVersion+" (reported to the backend)")
	}
	if e.codexModel != "" {
		line("", "model from its config.toml: "+e.codexModel)
	}

	if e.openAIKeyEnv != "" {
		line("OpenAI API key", "found in "+e.openAIKeyEnv)
	} else {
		line("OpenAI API key", "none (not required)")
	}

	if e.portFree {
		line("Listen address", e.listen+" is free")
	} else {
		line("Listen address", e.listen+" is IN USE - is a gateway already running?")
	}
}

// prompter asks questions on a terminal.
type prompter struct {
	in        *bufio.Scanner
	assumeYes bool
}

func newPrompter(assumeYes bool) (*prompter, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		if assumeYes {
			return &prompter{assumeYes: true}, nil
		}
		return nil, fmt.Errorf("setup needs a terminal; run it directly, add -y to accept every detected default, or write the config by hand with 'ccgw init'")
	}
	return &prompter{in: bufio.NewScanner(os.Stdin), assumeYes: assumeYes}, nil
}

func (p *prompter) ask(question, fallback string) string {
	if p.assumeYes {
		return fallback
	}
	fmt.Printf("%s [%s]: ", question, fallback)
	if !p.in.Scan() {
		return fallback
	}
	if answer := strings.TrimSpace(p.in.Text()); answer != "" {
		return answer
	}
	return fallback
}

func (p *prompter) yesNo(question string, fallback bool) bool {
	if p.assumeYes {
		return fallback
	}
	hint := "y/N"
	if fallback {
		hint = "Y/n"
	}
	for {
		fmt.Printf("%s [%s]: ", question, hint)
		if !p.in.Scan() {
			return fallback
		}
		switch strings.ToLower(strings.TrimSpace(p.in.Text())) {
		case "":
			return fallback
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Println("  please answer y or n")
	}
}

// answers is what setup will write.
type answers struct {
	listen       string
	useCodex     bool
	codexVersion string
	codexModels  []string
	// codexWindow is the real input context window of the Codex models, which
	// 'ccgw env' turns into the variable that corrects Claude Code's 200k
	// assumption. Zero leaves it unstated.
	codexWindow  int
	useOpenAI    bool
	openAIKeyEnv string
	openAIModels []string
}

func cmdSetup(args []string) error {
	fs := newFlagSet("setup")
	cfgPath := addConfigFlag(fs)
	force := fs.Bool("force", false, "overwrite an existing config without asking")
	listen := fs.String("listen", config.DefaultListen, "address the gateway should bind")
	assumeYes := fs.Bool("y", false, "accept every detected default without asking")
	codexWindow := fs.Int("codex-window", codex.DefaultContextWindow,
		"real input context window of the Codex models in tokens; 0 leaves it unstated")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p, err := newPrompter(*assumeYes)
	if err != nil {
		return err
	}

	fmt.Println("Looking at what is already set up on this machine...")
	fmt.Println()
	env := detect(*listen)
	env.report()
	fmt.Println()

	if _, err := os.Stat(*cfgPath); err == nil && !*force {
		// -y means "take every detected default", and the default answer to
		// "Replace it?" is no - so -y alone can never get past this. Say which
		// flag does rather than leaving that to be guessed.
		if p.assumeYes {
			return fmt.Errorf("a config already exists at %s - pass -force to replace it "+
				"(the previous file is kept as %s.bak)", *cfgPath, *cfgPath)
		}
		fmt.Printf("A config already exists at %s\n", *cfgPath)
		if !p.yesNo("Replace it?", false) {
			return fmt.Errorf("left the existing config alone")
		}
		fmt.Println()
	}

	a := answers{listen: *listen, codexWindow: *codexWindow}
	if err := planCodex(p, env, &a); err != nil {
		return err
	}
	planOpenAI(p, env, &a)

	body := renderConfig(a)
	fmt.Println()
	for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
		fmt.Println("  " + line)
	}
	fmt.Println()
	if !p.yesNo(fmt.Sprintf("Write this to %s?", *cfgPath), true) {
		return fmt.Errorf("nothing written")
	}

	if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o755); err != nil {
		return err
	}
	// setup regenerates the file from detection, so anything hand-tuned in the
	// old one is about to be lost. Keep a copy.
	backup, err := backupExisting(*cfgPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(*cfgPath, []byte(body), 0o644); err != nil {
		return err
	}
	if backup != "" {
		fmt.Printf("\nkept the previous config as %s\n", backup)
	}
	// A config this tool generated should never be one it then refuses to load.
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return fmt.Errorf("wrote %s but it does not load: %w", *cfgPath, err)
	}
	fmt.Printf("\nwrote %s\n", *cfgPath)

	// Write the picker cache now rather than leaving it to the next `serve`.
	// An already-running gateway does not re-read its config, so without this
	// the models just configured would not reach the picker until the gateway
	// happened to be restarted — which looks exactly like setup not working.
	if res, err := syncPicker(cfg, *cfgPath); err != nil {
		fmt.Printf("could not write the model picker: %v\n", err)
		fmt.Println("run 'ccgw sync-picker' once the gateway config is settled")
	} else {
		fmt.Printf("wrote %s (%d models)\n", res.Path, res.Count)
	}

	printNextSteps(a, env)
	return nil
}

// backupExisting copies path to path+".bak" when it exists, returning the
// backup's name, or "" when there was nothing to keep.
func backupExisting(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the config being replaced: %w", err)
	}
	dest := path + ".bak"
	if err := os.WriteFile(dest, raw, 0o600); err != nil {
		return "", fmt.Errorf("back up the existing config: %w", err)
	}
	return dest, nil
}

// planCodex decides the Codex half, asking only about what detection missed.
func planCodex(p *prompter, env environment, a *answers) error {
	switch {
	case env.codexSignedIn:
		// Already usable. Nothing to ask unless the model is unknown.
		a.useCodex = true
	case env.codexInstalled:
		fmt.Println("The Codex CLI is installed but this gateway found no ChatGPT sign-in.")
		a.useCodex = p.yesNo("Sign in to ChatGPT now? (opens a browser)", true)
	default:
		fmt.Println("No ChatGPT sign-in was found on this machine.")
		a.useCodex = p.yesNo("Use a ChatGPT Plus/Pro subscription for GPT models?", false)
	}
	if !a.useCodex {
		return nil
	}

	if !env.codexSignedIn {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		res, err := codex.Login(ctx, env.codexAuthPath, true, os.Stderr)
		if err != nil {
			return fmt.Errorf("sign-in failed: %w (retry with 'ccgw codex login')", err)
		}
		fmt.Printf("  signed in, wrote %s\n", res.Path)
		if res.PlanType != "" {
			fmt.Printf("  plan: %s\n", res.PlanType)
		}
	}

	// The installed CLI's version is the one the account is known to accept.
	a.codexVersion = firstNonEmpty(env.codexVersion, codex.DefaultClientVersion)

	// The whole known catalogue is configured, plus whatever the Codex CLI is
	// itself set to if that is something else. Availability is the account's
	// business: a model it does not serve is refused when selected, and having
	// the row present costs nothing until then.
	a.codexModels = codex.DefaultModelIDs()
	if env.codexModel != "" && !contains(a.codexModels, env.codexModel) {
		a.codexModels = append(a.codexModels, env.codexModel)
	}
	return nil
}

// planOpenAI adds a metered OpenAI provider only when a key is already present.
func planOpenAI(p *prompter, env environment, a *answers) {
	if env.openAIKeyEnv == "" {
		return // nothing detected, and a key is not something to prompt for
	}
	fmt.Println()
	fmt.Printf("%s is set. That is a metered platform key, billed separately from\n", env.openAIKeyEnv)
	fmt.Println("any ChatGPT subscription.")
	a.useOpenAI = p.yesNo("Add OpenAI API models too?", false)
	if !a.useOpenAI {
		return
	}
	a.openAIKeyEnv = env.openAIKeyEnv
	a.openAIModels = splitList(p.ask("OpenAI models (comma separated)", "gpt-5.6"))
}

func printNextSteps(a answers, env environment) {
	fmt.Println()
	fmt.Println("Next steps")
	step := 1
	next := func(format string, args ...any) {
		fmt.Printf("  %d. %s\n", step, fmt.Sprintf(format, args...))
		step++
	}
	if env.claudeInstalled && !env.claudeLoggedIn {
		next("claude login             # so Claude models use your subscription")
	}
	if env.claudeKeySource != "" {
		next("unset %s   # it overrides your Claude subscription", env.claudeKeySource)
	}
	// The ChatGPT credential is the one thing setup can finish without: it
	// writes a valid config either way. Listing no step for it reads as
	// "handled" rather than "skipped", which is how a gateway with no GPT
	// models looks like a gateway that worked.
	if !a.useCodex {
		next("ccgw codex login         # sign in to ChatGPT for the GPT models")
		next("ccgw setup -force        # re-run to add them to the config")
	}
	next("ccgw serve               # leave running")
	next(`eval "$(ccgw env)" && claude    # in another terminal`)
	if a.codexWindow > 0 && len(a.codexModels) > 0 {
		fmt.Println()
		fmt.Printf("  'ccgw env' also exports the %d-token context window of the Codex models,\n", a.codexWindow)
		fmt.Println("  so Claude Code stops compacting at the 200k it assumes for an unknown ID.")
		fmt.Println("  Launching claude without that eval leaves you on the 200k assumption.")
	}
	if a.useCodex {
		fmt.Println()
		fmt.Println("  The ChatGPT sign-in is already in place, so there is no codex login step.")
		fmt.Println("  'ccgw codex status' shows the plan, account and token expiry; only a dead")
		fmt.Println("  grant needs 'ccgw codex login' again.")
	}
	if len(a.codexModels) == 0 && len(a.openAIModels) == 0 {
		fmt.Println()
		fmt.Println("  No provider models were configured, so every model still routes to")
		fmt.Println("  Anthropic and the gateway changes nothing yet.")
	}
	fmt.Println()
	fmt.Println("  Then open /model and pick one of the new rows. Claude Code reads the")
	fmt.Println("  picker only at startup, so restart it fully after changing models.")
}

// renderConfig builds the YAML. It is written out rather than marshalled so the
// file the user opens next is commented and ordered for reading.
func renderConfig(a answers) string {
	var b strings.Builder
	b.WriteString("# ccgw - local LLM gateway for Claude Code\n")
	b.WriteString("# Generated by 'ccgw setup'. Edit freely.\n\n")
	fmt.Fprintf(&b, "listen: %s\n\n", a.listen)

	b.WriteString("anthropic:\n")
	b.WriteString("  base_url: https://api.anthropic.com\n")
	b.WriteString("  # passthrough relays the credential Claude Code sent, which is what\n")
	b.WriteString("  # keeps your Claude subscription paying for Claude models.\n")
	b.WriteString("  auth: passthrough\n\n")

	b.WriteString("# Non-Anthropic model IDs are advertised under this prefix, because Claude\n")
	b.WriteString("# Code drops any ID that does not contain \"claude\" or \"anthropic\". The\n")
	b.WriteString("# prefix is stripped again before the request reaches the provider.\n")
	fmt.Fprintf(&b, "alias_prefix: %q\n\n", config.DefaultAliasPrefix)

	b.WriteString("# Claude Code stops loading MCP tool schemas on demand behind a base URL\n")
	b.WriteString("# that is not Anthropic's own, and inlines every one of them instead - the\n")
	b.WriteString("# largest context cost the gateway imposes. Set this to false if a backend\n")
	b.WriteString("# answers 400 to the request shape it produces.\n")
	b.WriteString("enable_tool_search: true\n\n")

	b.WriteString("# Turn this on to have Claude Code treat the gateway's base URL as if it\n")
	b.WriteString("# were api.anthropic.com: remote managed settings are fetched again - it\n")
	b.WriteString("# refuses to behind a custom base URL - and Claude models keep their native\n")
	b.WriteString("# 1M window. It turns gateway model discovery off, so the models below reach\n")
	b.WriteString("# /model through a curated settings file that Claude Code has to be passed\n")
	b.WriteString("# with --settings; 'ccgw env' emits an alias for it. Fidelity mode only.\n")
	b.WriteString("# Internal Claude Code flag; may break.\n")
	b.WriteString("assume_first_party: false\n\n")

	if !a.useCodex && !a.useOpenAI {
		b.WriteString("providers: []\n\nmodels: []\n\n")
	} else {
		b.WriteString("providers:\n")
		if a.useCodex {
			b.WriteString("  - name: codex\n")
			b.WriteString("    type: codex\n")
			b.WriteString("    # The backend gates model availability on this. Raise it to match\n")
			b.WriteString("    # `codex --version` if a model is refused as needing a newer client.\n")
			fmt.Fprintf(&b, "    client_version: %q\n", a.codexVersion)
		}
		if a.useOpenAI {
			b.WriteString("  - name: openai\n")
			b.WriteString("    type: openai\n")
			b.WriteString("    base_url: https://api.openai.com/v1\n")
			fmt.Fprintf(&b, "    api_key_env: %s\n", a.openAIKeyEnv)
		}
		b.WriteString("\nmodels:\n")
		if len(a.codexModels) > 0 && a.codexWindow > 0 {
			b.WriteString("  # context_window is the measured input limit of the backend. Claude\n")
			b.WriteString("  # Code assumes 200k for an ID it does not recognise, so without this\n")
			b.WriteString("  # it compacts at a fraction of what the account serves. 'ccgw env'\n")
			b.WriteString("  # exports the variable that corrects it. Re-measure and lower this if\n")
			b.WriteString("  # a long session starts being refused.\n")
		}
		for _, m := range a.codexModels {
			fmt.Fprintf(&b, "  - id: %q\n    provider: codex\n", m)
			fmt.Fprintf(&b, "    display_name: %q\n", firstNonEmpty(codex.LabelFor(m), displayName(m, "Codex")))
			if d := codex.DescriptionFor(m); d != "" {
				fmt.Fprintf(&b, "    description: %q\n", d)
			}
			if a.codexWindow > 0 {
				fmt.Fprintf(&b, "    context_window: %d\n", a.codexWindow)
			}
		}
		for _, m := range a.openAIModels {
			fmt.Fprintf(&b, "  - id: %q\n    provider: openai\n    display_name: %q\n", m, displayName(m, "OpenAI"))
		}
		b.WriteString("\n")
	}
	b.WriteString("# Any model ID not listed above goes to Anthropic unchanged, so the Claude\n")
	b.WriteString("# models never need enumerating here.\n")
	return b.String()
}

// acronyms are rendered uppercase in a picker label rather than title-cased,
// so "gpt-5.6-terra" reads as "GPT 5.6 Terra" and not "Gpt 5.6 terra".
var acronyms = map[string]string{
	"gpt": "GPT", "o1": "o1", "o3": "o3", "o4": "o4", "api": "API",
}

// displayName makes a readable picker label from a model ID.
func displayName(id, suffix string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, w := range words {
		switch {
		case acronyms[strings.ToLower(w)] != "":
			words[i] = acronyms[strings.ToLower(w)]
		case w == "":
		case w[0] >= '0' && w[0] <= '9':
			// A version fragment stays as written.
		default:
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	label := strings.Join(words, " ")
	if label == "" {
		label = id
	}
	return fmt.Sprintf("%s (%s)", label, suffix)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
