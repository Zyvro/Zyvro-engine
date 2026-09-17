package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Compléter le code sous le curseur.
//
// C'est un quatrième métier, et pas une variante du texte. Un modèle de chat
// répond une phrase — « Voici la fonction que vous cherchez : » — là où
// l'éditeur attend trois caractères à insérer entre ce qui précède et ce qui
// suit. Le geste s'appelle *fill-in-the-middle*, il a son propre appel, et les
// modèles qui le savent faire ne sont pas les mêmes.
//
// D'où une catégorie à part plutôt qu'une case à cocher dans la génération de
// texte : la liste des dos n'est pas la même, la liste des modèles non plus, et
// mélanger les deux donnerait un réglage qui a l'air de marcher et qui rend des
// phrases.
//
// Le protocole : `POST /v1/completions` avec `prompt` et `suffix`. C'est
// l'ancienne route de complétion d'OpenAI, celle que llama.cpp, LM Studio,
// vLLM et Ollama servent tous — vérifié contre l'Ollama de cette machine, qui
// répond « does not support insert » quand le modèle n'est pas entraîné pour,
// ce qui prouve au passage que le `suffix` lui parvient.

// CompletionProviders sont les dos qui savent insérer au milieu.
//
// Les trois serveurs locaux, et Ollama hébergé qui sert la même route. Pas
// OpenAI : ses modèles actuels ne servent plus cette route, et l'y mettre
// promettrait une complétion qui revient en erreur.
var CompletionProviders = append(
	[]string{"ollama"},
	OpenAICompatibleProviders...,
)

// CompletionRequest est ce qu'un éditeur a sous la main au moment où il
// demande : ce qui précède le curseur, ce qui le suit, et jusqu'où aller.
type CompletionRequest struct {
	Provider string
	Model    string
	Prefix   string
	Suffix   string
	// MaxTokens est petit par nature. Une complétion en ligne se lit d'un coup
	// d'œil ; au-delà, elle a dépassé la question qu'on lui posait, et elle
	// coûte le temps qu'elle met à arriver.
	MaxTokens int
}

// DefaultCompletionTokens est la longueur d'une complétion quand personne ne
// dit. Assez pour une ligne et le début de la suivante.
const DefaultCompletionTokens = 96

func (c *Config) resolveCompletionProvider(requested string) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "ollama":
		return "ollama"
	case OllamaLocalProvider:
		return OllamaLocalProvider
	case LMStudioProvider:
		return LMStudioProvider
	case CustomProvider:
		return CustomProvider
	}
	// Le local d'abord, et c'est un choix, pas un hasard : une complétion doit
	// revenir en quelques dizaines de millisecondes pour être utile, ce qu'un
	// aller-retour vers un service hébergé ne tient pas.
	return c.PreferredOrDefault("completion", c.hasCompletionCredential, func() string {
		for _, p := range OpenAICompatibleProviders {
			if c.EndpointConfigured(p) {
				return p
			}
		}
		return "ollama"
	})
}

func (c *Config) hasCompletionCredential(provider string) bool {
	switch provider {
	case "ollama":
		return c.hasOllama()
	case OllamaLocalProvider, LMStudioProvider, CustomProvider:
		return c.EndpointConfigured(provider)
	}
	return false
}

// ResolvedCompletionProvider dit où une demande atterrirait, pour qu'un appelant
// puisse décider quoi envoyer avec — un nom de modèle, en particulier.
func (c *Config) ResolvedCompletionProvider(requested string) string {
	return c.resolveCompletionProvider(requested)
}

// CompletionCredential rend de quoi vérifier qu'un dos est utilisable avant de
// lancer quoi que ce soit. Il suit ImageCredential et VisionCredential.
func (c *Config) CompletionCredential(provider string) string {
	switch p := c.resolveCompletionProvider(provider); p {
	case "ollama":
		if key := strings.TrimSpace(c.OllamaAPIKey); key != "" {
			return key
		}
		if url := strings.TrimSpace(c.OllamaURL); url != DefaultOllamaURL {
			return url
		}
		return ""
	case OllamaLocalProvider, LMStudioProvider, CustomProvider:
		return c.endpointFor(p).URL
	}
	return ""
}

type completionPayload struct {
	Model       string   `json:"model"`
	Prompt      string   `json:"prompt"`
	Suffix      string   `json:"suffix,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature float64  `json:"temperature"`
	Stop        []string `json:"stop,omitempty"`
}

type completionAnswer struct {
	Choices []struct {
		Text         string `json:"text"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// CodeCompletion demande ce qui va entre le préfixe et le suffixe.
func (c *Config) CodeCompletion(ctx context.Context, req CompletionRequest) (string, error) {
	provider := c.resolveCompletionProvider(req.Provider)

	base, key, err := c.completionEndpoint(provider)
	if err != nil {
		return "", err
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = c.completionModelFor(provider)
	}
	if model == "" {
		return "", fmt.Errorf("%s has no completion model chosen — pick one in the provider settings", provider)
	}
	if strings.TrimSpace(req.Prefix) == "" && strings.TrimSpace(req.Suffix) == "" {
		// Rien autour du curseur : il n'y a pas de milieu à remplir, et
		// demander quand même ferait écrire au modèle un fichier entier.
		return "", nil
	}

	max := req.MaxTokens
	if max <= 0 {
		max = DefaultCompletionTokens
	}
	body, err := json.Marshal(completionPayload{
		Model:     model,
		Prompt:    req.Prefix,
		Suffix:    req.Suffix,
		MaxTokens: max,
		// Zéro, et c'est délibéré : une complétion en ligne doit être la même
		// à chaque frappe. Une suggestion qui change toute seule est une
		// suggestion qu'on n'ose plus accepter.
		Temperature: 0,
		// Deux lignes vides : le modèle a fini son idée, la suite est du
		// remplissage qu'on paierait à l'afficher.
		Stop: []string{"\n\n\n"},
	})
	if err != nil {
		return "", err
	}

	raw, err := postJSON(ctx, base+"/completions", key, body)
	if err != nil {
		return "", completionError(err, model)
	}

	var parsed completionAnswer
	if json.Unmarshal(raw, &parsed) != nil {
		return "", fmt.Errorf("that address did not answer with a completion")
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return "", completionMessage(parsed.Error.Message, model)
	}
	if len(parsed.Choices) == 0 {
		return "", nil
	}
	return trimCompletion(parsed.Choices[0].Text), nil
}

// completionEndpoint dit où va la demande et ce qui l'ouvre.
func (c *Config) completionEndpoint(provider string) (base, key string, err error) {
	switch provider {
	case OllamaLocalProvider, LMStudioProvider, CustomProvider:
		e := c.endpointFor(provider)
		if e.URL == "" {
			return "", "", fmt.Errorf("%s has no address configured", provider)
		}
		return e.URL, e.Key, nil
	case "ollama":
		// OllamaURL porte l'hôte sans le segment de version, contrairement aux
		// endpoints configurés à la main.
		host := strings.TrimSuffix(strings.TrimSpace(c.OllamaURL), "/")
		if host == "" {
			host = DefaultOllamaURL
		}
		return host + "/v1", strings.TrimSpace(c.OllamaAPIKey), nil
	}
	return "", "", fmt.Errorf("%s cannot complete code", provider)
}

// completionModelFor : le modèle du serveur choisi, jamais celui d'un autre.
//
// Rien ne tombe sur `OllamaModel` ici. Ce nom-là est celui d'un modèle de chat,
// et un modèle de chat répond une phrase à une demande de complétion : la
// suggestion s'afficherait en texte fantôme, elle serait grammaticalement
// correcte, et elle ne compilerait jamais.
func (c *Config) completionModelFor(provider string) string {
	switch provider {
	case OllamaLocalProvider, LMStudioProvider, CustomProvider:
		return strings.TrimSpace(c.endpointFor(provider).Model)
	}
	return ""
}

// completionError traduit ce que le serveur a dit en ce qu'il faut faire.
func completionError(err error, model string) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return err
	}
	return completionMessage(pe.Message, model)
}

// completionMessage : le cas qui arrivera à tout le monde une fois.
//
// Ollama répond « does not support insert » quand le modèle n'a pas été
// entraîné à remplir un milieu — ce qui est le cas de presque tous les modèles
// de chat. Laisser passer cette phrase telle quelle enverrait chercher du côté
// du serveur ; ce qu'il faut, c'est un modèle de code.
func completionMessage(message, model string) error {
	if strings.Contains(strings.ToLower(message), "does not support insert") {
		return fmt.Errorf(
			"%s cannot fill in the middle of a file — choose a code model trained for it, such as qwen2.5-coder or deepseek-coder",
			model,
		)
	}
	return &ProviderError{Code: "provider_error", Message: truncate(message, 300)}
}

// trimCompletion enlève ce qu'un éditeur ne veut pas insérer.
//
// Les espaces de fin, d'abord : le curseur est déjà là où il faut, et une
// suggestion qui traîne deux espaces derrière elle les laisse dans le fichier.
// Les retours à la ligne de tête, ensuite, mais seulement eux : l'indentation
// d'une suggestion est une partie de la suggestion.
func trimCompletion(text string) string {
	out := strings.TrimRight(text, " \t\r\n")
	return strings.TrimLeft(out, "\r\n")
}
