package providers

import "strings"

// The order a person wants their own providers tried in.
//
// Every router here ends the same way: when nothing named a provider, pick
// whichever credential exists. That rule had to choose an order, and the order
// was written into the code — google before bfl for images, google before
// ollama before openai for vision. It was a guess about what somebody would
// want, made by whoever wrote the line, and it was invisible to the person it
// decided for.
//
// So it becomes a preference instead. A person with two image accounts says
// which one goes first; a person with one is unaffected and never sees the
// question. The routers consult this before their own fallback, which stays as
// the answer for somebody who has expressed no preference at all.
//
// Per role rather than one global list, because the roles are genuinely
// different questions: the account you want images billed to is not
// necessarily the one you want reading a screenshot.

// Preference is an ordered list of provider ids per role. The roles are the
// ones the three routers serve: "text", "image", "vision".
type Preference map[string][]string

// firstAvailable walks a role's preference and answers the first provider that
// has a credential, or "" when none of them do — which is when the caller falls
// back to its own rule.
//
// `has` is passed in rather than read here because what counts as a credential
// differs by role: a local Ollama needs none, and a CLI provider's "credential"
// is a binary on the PATH.
func (c *Config) firstAvailable(role string, has func(provider string) bool) string {
	for _, p := range c.Preference[role] {
		id := strings.ToLower(strings.TrimSpace(p))
		if id == "" {
			continue
		}
		if has(id) {
			return id
		}
	}
	return ""
}

// ProvidersFor is the set of providers that can do one job.
//
// One place, because the three routers, the daemon's catalogue and the hosted
// service all ask this same question, and the copy that is written down a
// fourth time is the one that is wrong the day a provider is added.
func ProvidersFor(role string) []string {
	switch role {
	case "text":
		return TextProviders
	case "image":
		return ImageProviders
	case "vision":
		return VisionProviders
	case "completion":
		return CompletionProviders
	}
	return nil
}

// PreferredOrDefault is the shape every router uses: a person's order first,
// their own rule second. Kept in one place so the three cannot drift into
// disagreeing about what a preference means.
//
// The third step is the one that had to be added. Each router's own rule names
// a fixed provider — "ollama" for text, "google" for images and for vision —
// and a fixed name is a guess that stops being true the moment somebody's only
// backend is one the rule has never heard of. Somebody running LM Studio on
// their laptop and holding no key at all was told to go and get an Ollama key,
// which is exactly the complaint that made vision stop being Gemini's alone,
// reappearing one layer down.
//
// So: a rule that names something usable is still obeyed, and only a rule that
// names something this run cannot use gives way to whatever it can. The list
// order decides, and every list puts the hosted backends first, so nothing
// changes for a deployment that has one.
func (c *Config) PreferredOrDefault(role string, has func(string) bool, fallback func() string) string {
	if picked := c.firstAvailable(role, has); picked != "" {
		return picked
	}
	picked := fallback()
	if has(picked) {
		return picked
	}
	for _, p := range ProvidersFor(role) {
		if IsCLIProvider(p) {
			// Not chosen for somebody. A pasted key or a typed address is an
			// act of configuration inside this app; a command line tool on the
			// PATH was installed for something else, and spending a person's
			// subscription because a binary happens to exist is a decision
			// that belongs to them. Named by a node or by their own order, it
			// runs — that is what naming it means.
			continue
		}
		if has(p) {
			return p
		}
	}
	// Nothing at all is configured. The rule's own answer is the right one to
	// return: it is what the "no key for this" error will name, and naming the
	// backend the deployment expects is more use than naming the last entry of
	// a list.
	return picked
}
