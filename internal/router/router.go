// Package router maps the model ID on an incoming request to a backend.
package router

import (
	"fmt"
	"strings"

	"github.com/bfreis/claude-code-llm-gateway/internal/config"
)

// Kind distinguishes the two routing outcomes.
type Kind int

const (
	// KindAnthropic proxies the request to the Anthropic API unchanged.
	KindAnthropic Kind = iota
	// KindProvider translates the request for a non-Anthropic backend.
	KindProvider
)

// Route is the resolved destination for one request.
type Route struct {
	Kind Kind
	// Provider is set when Kind is KindProvider.
	Provider *config.Provider
	// Model is the configured catalogue entry, set when Kind is KindProvider.
	Model config.Model
	// UpstreamModel is the model ID to send to the backend. For Anthropic it
	// is the incoming ID verbatim, variant suffix included.
	UpstreamModel string
}

// Router resolves model IDs against the configured catalogue.
type Router struct {
	cfg    *config.Config
	byID   map[string]entry
	prefix string
}

type entry struct {
	provider *config.Provider
	model    config.Model
}

// New builds a Router. The config must already have been validated.
func New(cfg *config.Config) *Router {
	r := &Router{cfg: cfg, byID: map[string]entry{}, prefix: cfg.Prefix()}
	for _, m := range cfg.Models {
		p, ok := cfg.ProviderByName(m.Provider)
		if !ok {
			continue // validate() already rejected this
		}
		e := entry{provider: p, model: m}
		// Accept both the prefixed ID (what the picker selects) and the bare
		// ID (what someone types into --model).
		r.byID[r.ExposedID(m.ID)] = e
		r.byID[m.ID] = e
	}
	return r
}

// ExposedID is the ID advertised to Claude Code for a provider model.
//
// The prefix exists only to satisfy Claude Code's discovery filter, which drops
// any model ID not matching /(claude|anthropic)/i. An ID that already satisfies
// the filter is left alone.
func (r *Router) ExposedID(modelID string) string {
	if r.prefix == "" || containsClaudeOrAnthropic(modelID) {
		return modelID
	}
	return r.prefix + modelID
}

func containsClaudeOrAnthropic(id string) bool {
	l := strings.ToLower(id)
	return strings.Contains(l, "claude") || strings.Contains(l, "anthropic")
}

// Resolve picks the backend for an incoming model ID.
//
// Unknown IDs fall through to Anthropic, which keeps every real Claude model
// working without having to be enumerated in the config.
func (r *Router) Resolve(modelID string) Route {
	if e, ok := r.byID[modelID]; ok {
		return Route{Kind: KindProvider, Provider: e.provider, Model: e.model, UpstreamModel: e.model.ID}
	}
	// Claude Code appends a variant marker for long-context rows, e.g.
	// "claude-sonnet-5[1m]". Match the base ID, but only for our own models:
	// Anthropic understands the suffix itself.
	if base, _, ok := splitVariant(modelID); ok {
		if e, found := r.byID[base]; found {
			return Route{Kind: KindProvider, Provider: e.provider, Model: e.model, UpstreamModel: e.model.ID}
		}
	}
	return Route{Kind: KindAnthropic, UpstreamModel: modelID}
}

// splitVariant splits "id[variant]" into its parts.
func splitVariant(id string) (base, variant string, ok bool) {
	if !strings.HasSuffix(id, "]") {
		return id, "", false
	}
	i := strings.LastIndex(id, "[")
	if i <= 0 {
		return id, "", false
	}
	return id[:i], id[i+1 : len(id)-1], true
}

// Catalogue returns the configured provider models, in config order, with the
// IDs Claude Code should see.
func (r *Router) Catalogue() []CatalogueEntry {
	out := make([]CatalogueEntry, 0, len(r.cfg.Models))
	for _, m := range r.cfg.Models {
		display := m.DisplayName
		if display == "" {
			display = m.ID
		}
		id := r.ExposedID(m.ID)
		if m.LongContext {
			// Claude Code reads this suffix as a client-side window hint; the
			// router strips it again when the request comes back.
			id += "[1m]"
		}
		out = append(out, CatalogueEntry{
			ID:          id,
			DisplayName: display,
			Description: m.Description,
		})
	}
	return out
}

// DiscoveryWarnings reports configured models that Claude Code's gateway
// discovery will silently drop from the /model picker.
//
// Discovery keeps a model only when its ID matches /(claude|anthropic)/i AND
// the ID is not an exact, case-insensitive match for one of the provider
// spellings of a model in Claude Code's own baked catalog (the one exception
// being the fable family). The first half is checkable here; the second is not,
// because the catalog ships inside Claude Code and drifts with its releases —
// but it only bites when an advertised ID is spelled exactly like a real Claude
// model, which the default alias prefix already prevents.
func (r *Router) DiscoveryWarnings() []string {
	var out []string
	for _, m := range r.cfg.Models {
		id := r.ExposedID(m.ID)
		if !containsClaudeOrAnthropic(id) {
			out = append(out, fmt.Sprintf(
				"model %q is advertised as %q, which Claude Code's discovery will drop: "+
					"the ID must contain \"claude\" or \"anthropic\". Set alias_prefix, or rename the model.",
				m.ID, id))
		}
	}
	return out
}

// CatalogueEntry is one advertised model.
type CatalogueEntry struct {
	ID          string
	DisplayName string
	Description string
}
