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

// PreferredOrDefault is the shape every router uses: a person's order first,
// their own rule second. Kept in one place so the three cannot drift into
// disagreeing about what a preference means.
func (c *Config) PreferredOrDefault(role string, has func(string) bool, fallback func() string) string {
	if picked := c.firstAvailable(role, has); picked != "" {
		return picked
	}
	return fallback()
}
