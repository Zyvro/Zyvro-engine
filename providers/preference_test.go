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
	// An account that said nothing gets the deployment's own rule — as long as
	// that rule names something this run can use. It used to be obeyed even
	// when it named a provider with no credential, which sent an account
	// holding two working keys to a third backend it had never configured.
	plain := &Config{AnthropicAPIKey: "a", OpenAIAPIKey: "x"}
	if got := plain.resolveTextProvider(""); got != "anthropic" {
		t.Fatalf("text went to %q rather than to a backend this run can use", got)
	}
	// And the rule is still the rule when it is usable: a run carrying the
	// lent Ollama key keeps going to Ollama, which is what pays for it.
	lent := &Config{AnthropicAPIKey: "a", OllamaAPIKey: "lent"}
	if got := lent.resolveTextProvider(""); got != "ollama" {
		t.Fatalf("the deployment's own rule was ignored: %q", got)
	}
}

// Somebody whose only backend is a server on their own machine.
//
// This is the complaint that made vision stop being Gemini's alone, one layer
// down: the routers' own rules name a fixed hosted provider, so a person
// running LM Studio and holding no key at all was told to go and get a key for
// a backend they had never asked for — for all three jobs.
func TestTheOnlyBackendSomebodyHasIsTheOneThatIsUsed(t *testing.T) {
	local := &Config{Endpoints: map[string]Endpoint{
		LMStudioProvider:    {Model: "qwen"},
		CustomImageProvider: {URL: "http://127.0.0.1:8000/v1"},
	}}
	if got := local.resolveTextProvider(""); got != LMStudioProvider {
		t.Errorf("text went to %q", got)
	}
	if got := local.resolveVisionProvider(""); got != LMStudioProvider {
		t.Errorf("vision went to %q", got)
	}
	if got := local.resolveImageProvider(""); got != CustomImageProvider {
		t.Errorf("image went to %q", got)
	}

	// A hosted credential still wins when there is one: every role's list puts
	// the hosted backends first, so nothing changes for a deployment that has
	// them.
	both := &Config{
		GoogleAPIKey: "g",
		Endpoints:    map[string]Endpoint{LMStudioProvider: {Model: "qwen"}},
	}
	if got := both.resolveVisionProvider(""); got != "google" {
		t.Errorf("vision left google for %q", got)
	}

	// And with nothing at all configured, the rule's own answer is what comes
	// back: it is what the "no key for this" error will name.
	empty := &Config{}
	if got := empty.resolveVisionProvider(""); got != "google" {
		t.Errorf("an empty config resolved to %q", got)
	}
	if got := empty.resolveTextProvider(""); got != "ollama" {
		t.Errorf("an empty config resolved to %q", got)
	}
}

// A command line tool on the PATH was installed for something else.
//
// Spending somebody's Claude or ChatGPT subscription because a binary happens
// to exist is a decision that belongs to them, and it would be taken silently:
// nothing in the app would say why a text node started talking to a
// subprocess. Named by a node or by their own order it runs, which is what
// naming it means.
func TestAnInstalledCLIIsNeverChosenUnasked(t *testing.T) {
	// Both installed and signed in, and nothing else at all configured.
	installed := fakeCLI(t, "echo '{}'")
	c := &Config{ClaudeCLIPath: installed, CodexCLIPath: installed}
	if c.TextCredentialFor("claude-cli") == "" {
		t.Fatal("the fixture did not make the CLI look installed")
	}
	if got := c.resolveTextProvider(""); got != "ollama" {
		t.Errorf("an installed CLI was picked unasked: %q", got)
	}

	// Asked for, it is used.
	if got := c.resolveTextProvider("claude-cli"); got != "claude-cli" {
		t.Errorf("a node naming the CLI went to %q", got)
	}
	named := &Config{ClaudeCLIPath: installed, Preference: Preference{"text": {"claude-cli"}}}
	if got := named.resolveTextProvider(""); got != "claude-cli" {
		t.Errorf("an order naming the CLI went to %q", got)
	}
}
