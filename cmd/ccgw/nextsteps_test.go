package main

import (
	"strings"
	"testing"
)

func nextSteps(t *testing.T, a answers, env environment) string {
	t.Helper()
	return captureStdout(t, func() error {
		printNextSteps(a, env)
		return nil
	})
}

func TestNextStepsOfferCodexLoginWhenItIsNotConfigured(t *testing.T) {
	// Declining, or running -y with no sign-in detected, leaves the gateway
	// with no GPT models. Saying nothing makes that look like it worked.
	out := nextSteps(t, answers{listen: "127.0.0.1:8787"}, environment{codexInstalled: true})
	// ". " is how a numbered step is printed, so this asserts a step rather
	// than a passing mention in prose.
	if !strings.Contains(out, ". ccgw codex login") {
		t.Errorf("no sign-in step offered:\n%s", out)
	}
	if !strings.Contains(out, "setup -force") {
		t.Errorf("did not say how to get the models into the config:\n%s", out)
	}
}

func TestNextStepsSayWhyThereIsNoCodexLoginStep(t *testing.T) {
	// When the credential is already good there is nothing to do, but an
	// absent step is indistinguishable from an oversight - so say so.
	out := nextSteps(t,
		answers{listen: "127.0.0.1:8787", useCodex: true, codexModels: []string{"gpt-5.6-sol"}},
		environment{codexSignedIn: true})
	if strings.Contains(out, ". ccgw codex login") {
		t.Errorf("offered a sign-in that is already done:\n%s", out)
	}
	if !strings.Contains(out, "ccgw codex status") {
		t.Errorf("never mentions the Codex credential at all:\n%s", out)
	}
}

func TestNextStepsFlagAConfigWithNoProviderModels(t *testing.T) {
	// This config is valid and does nothing: every model still goes to
	// Anthropic, which is exactly what running without the gateway does.
	out := nextSteps(t, answers{listen: "127.0.0.1:8787"}, environment{})
	if !strings.Contains(out, "No provider models") {
		t.Errorf("an empty catalogue was not called out:\n%s", out)
	}
}

func TestNextStepsKeepTheClaudeLoginStep(t *testing.T) {
	out := nextSteps(t,
		answers{listen: "127.0.0.1:8787", useCodex: true, codexModels: []string{"gpt-5.6-sol"}},
		environment{claudeInstalled: true, codexSignedIn: true})
	if !strings.Contains(out, "claude login") {
		t.Errorf("lost the Claude sign-in step:\n%s", out)
	}
}
