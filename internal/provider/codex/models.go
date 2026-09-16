package codex

// Model is one entry in the default catalogue.
type Model struct {
	ID          string
	DisplayName string
	Description string
}

// DefaultModels are the Codex models setup configures out of the box.
//
// The IDs are the ones a Codex backend serves; the labels are written out
// rather than derived from the ID so the picker reads well. Availability still
// depends on the account and on the reported client version — a model the
// subscription does not serve is refused when selected, not when configured.
var DefaultModels = []Model{
	{ID: "gpt-6-astra", DisplayName: "GPT-6 Astra (Codex)", Description: "OpenAI GPT-6 Astra via your ChatGPT subscription"},
	{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol (Codex)", Description: "OpenAI GPT-5.6 Sol via your ChatGPT subscription"},
	{ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra (Codex)", Description: "OpenAI GPT-5.6 Terra via your ChatGPT subscription"},
	{ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna (Codex)", Description: "OpenAI GPT-5.6 Luna via your ChatGPT subscription"},
}

// DefaultModelIDs lists the default catalogue's IDs.
func DefaultModelIDs() []string {
	out := make([]string, len(DefaultModels))
	for i, m := range DefaultModels {
		out[i] = m.ID
	}
	return out
}

// LabelFor returns the curated label for a known ID, or "" for anything else.
func LabelFor(id string) string {
	for _, m := range DefaultModels {
		if m.ID == id {
			return m.DisplayName
		}
	}
	return ""
}

// DescriptionFor returns the curated description for a known ID.
func DescriptionFor(id string) string {
	for _, m := range DefaultModels {
		if m.ID == id {
			return m.Description
		}
	}
	return ""
}
