package providers

import "testing"

// The order a person wants their own providers tried in.
//
// Every router ends the same way: when nothing named a provider, pick whichever
// credential exists. That rule had to choose an order, and the order was
// written into the code — a guess about what somebody would want, made by
// whoever wrote the line, invisible to the person it decided for.
//
// What matters in these tests is the precedence. A preference must beat the
// built-in fallback and must lose to an explicit request, and a preference
// naming a provider the account has no credential for must be skipped rather
// than obeyed — otherwise expressing one could make a working account stop
// working.

func TestAPreferenceDecidesWhenNothingElseDoes(t *testing.T) {
	cfg := &Config{
		GoogleAPIKey: "g",
		BFLAPIKey:    "b",
		Preference:   Preference{"image": {"bfl", "google"}},
	}
	if got := cfg.resolveImageProvider(""); got != "bfl" {
		t.Fatalf("image went to %q despite the account asking for bfl first", got)
	}
	// Without the preference, the built-in rule prefers google.
	plain := &Config{GoogleAPIKey: "g", BFLAPIKey: "b"}
	if got := plain.resolveImageProvider(""); got != "google" {
		t.Fatalf("the fallback changed: %q", got)
	}
}

func TestAnExplicitRequestStillWins(t *testing.T) {
	cfg := &Config{
		GoogleAPIKey: "g",
		BFLAPIKey:    "b",
		Preference:   Preference{"image": {"bfl"}},
	}
	// A node that names a backend means it: the preference is what to do when
	// nobody said, not an override of what somebody did say.
	if got := cfg.resolveImageProvider("google"); got != "google" {
		t.Fatalf("an explicit choice was overridden by the preference: %q", got)
	}
}

func TestAPreferenceForAProviderWithNoKeyIsSkipped(t *testing.T) {
	// Somebody orders three, then removes the key for the first. Obeying the
	// order literally would send every call to an account that cannot answer,
	// so expressing a preference would have broken a working setup.
	cfg := &Config{
		OllamaAPIKey: "o",
		Preference:   Preference{"vision": {"openai", "google", "ollama"}},
	}
	if got := cfg.resolveVisionProvider(""); got != "ollama" {
		t.Fatalf("vision went to %q, want the only one with a credential", got)
	}
}

func TestAPreferenceThatNamesNothingUsableFallsBack(t *testing.T) {
	cfg := &Config{GoogleAPIKey: "g", Preference: Preference{"vision": {"openai"}}}
	if got := cfg.resolveVisionProvider(""); got != "google" {
		t.Fatalf("got %q, want the built-in rule's answer", got)
	}
	// Junk in the list is ignored rather than obeyed.
	messy := &Config{GoogleAPIKey: "g", BFLAPIKey: "b", Preference: Preference{"image": {"", "  ", "nonsense", "bfl"}}}
	if got := messy.resolveImageProvider(""); got != "bfl" {
		t.Fatalf("got %q, want bfl once the junk was stepped over", got)
	}
}

func TestEachRoleHasItsOwnOrder(t *testing.T) {
	// The account you want images billed to is not necessarily the one you want
	// reading a screenshot, which is the whole reason this is per role.
	cfg := &Config{
		GoogleAPIKey: "g",
		BFLAPIKey:    "b",
		OllamaAPIKey: "o",
		Preference: Preference{
			"image":  {"bfl"},
			"vision": {"ollama"},
		},
	}
	if got := cfg.resolveImageProvider(""); got != "bfl" {
		t.Fatalf("image = %q", got)
	}
	if got := cfg.resolveVisionProvider(""); got != "ollama" {
		t.Fatalf("vision = %q", got)
	}
}

func TestAnAccountWithNoPreferenceIsUnaffected(t *testing.T) {
	// Which is every account that has one credential for a job and therefore
	// never had to think about the question.
	for _, cfg := range []*Config{
		{GoogleAPIKey: "g"},
		{BFLAPIKey: "b"},
		{OllamaAPIKey: "o"},
		{},
	} {
		if cfg.resolveImageProvider("") == "" || cfg.resolveVisionProvider("") == "" || cfg.resolveTextProvider("") == "" {
			t.Fatal("a router answered with nothing at all")
		}
	}
}

func TestTextFollowsThePreferenceToo(t *testing.T) {
	cfg := &Config{
		AnthropicAPIKey: "a",
		OpenAIAPIKey:    "x",
		Preference:      Preference{"text": {"openai", "anthropic"}},
	}
	if got := cfg.resolveTextProvider(""); got != "openai" {
		t.Fatalf("text went to %q despite the account asking for openai first", got)
	}
	// And the old behaviour is intact for an account that said nothing.
	plain := &Config{AnthropicAPIKey: "a", OpenAIAPIKey: "x"}
	if got := plain.resolveTextProvider(""); got != "ollama" {
		t.Fatalf("the text fallback changed: %q", got)
	}
}
