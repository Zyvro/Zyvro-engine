package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Veo, par l'API Gemini.
//
// Ce n'est pas `generateContent` : la vidéo passe par `predictLongRunning`, qui
// rend le nom d'une opération plutôt qu'un résultat. On interroge ce nom
// jusqu'à ce qu'il se dise fait, et la vidéo est alors une URL à télécharger
// avec la même clé — c'est la seule route de ce moteur où la clé sert à lire un
// fichier et pas à demander un travail.
//
// Ce que Veo accepte est plus étroit que ce que Black Forest Labs accepte, et
// c'est écrit ici plutôt que découvert au retour : deux formats d'image, trois
// résolutions, trois durées. Une demande qui sort de là est refusée en nommant
// ce qui existe, parce qu'une minute d'attente pour s'entendre dire non est une
// minute facturée à la personne.

// veoDefaultModel est le modèle qu'un nœud obtient sans en nommer un.
const veoDefaultModel = "veo-3.1-generate-preview"

// veoModelPrefix est la seule chose qu'on vérifie du nom d'un modèle, avec les
// caractères qu'il contient.
//
// Une allow-list nominative comme celle de Black Forest Labs vieillirait mal :
// Google publie une version de Veo tous les quelques mois, et le nom exact
// n'est connu que le jour où elle sort. Un préfixe plus un alphabet suffit à ce
// qui compte — le nom devient un segment d'URL, et un nœud ne doit pas pouvoir
// nommer une autre route de l'hôte.
const veoModelPrefix = "veo-"

// veoPollInterval suit la recommandation de Google : dix secondes. Une vidéo
// prend des dizaines de secondes à rendre, donc interroger plus souvent
// n'avancerait rien et ferait du bruit pour rien.
//
// Une variable et non une constante pour que les tests puissent la raccourcir :
// vérifier la mécanique d'attente ne doit pas coûter dix secondes par cas.
var veoPollInterval = 10 * time.Second

// veoResolutions traduit le vocabulaire commun vers celui de Veo.
//
// QHD est absent, et c'est la vérité plutôt qu'un oubli : Veo rend du 720p, du
// 1080p et du 4k. Ramener silencieusement une demande de QHD à du 1080p
// donnerait une vidéo plus petite que demandé sans que rien ne le dise.
var veoResolutions = map[string]string{
	"hd":  "720p",
	"fhd": "1080p",
	"uhd": "4k",
}

// veoAspectRatios : deux, et deux seulement.
var veoAspectRatios = []string{"16:9", "9:16"}

// veoDurations : quatre, six ou huit secondes.
var veoDurations = []int{4, 6, 8}

type veoOperation struct {
	Name     string          `json:"name"`
	Done     bool            `json:"done"`
	Error    *veoStatusError `json:"error"`
	Response struct {
		GenerateVideoResponse struct {
			GeneratedSamples []struct {
				Video struct {
					URI string `json:"uri"`
				} `json:"video"`
			} `json:"generatedSamples"`
		} `json:"generateVideoResponse"`
	} `json:"response"`
}

type veoStatusError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Config) geminiVideoGenerate(ctx context.Context, req VideoRequest) (*VideoResult, error) {
	if strings.TrimSpace(c.GoogleAPIKey) == "" {
		return nil, fmt.Errorf("no Google AI Studio key is configured")
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = veoDefaultModel
	}
	if err := checkVeoModel(model); err != nil {
		return nil, err
	}

	resolution, err := normalizeResolution(req.Resolution)
	if err != nil {
		return nil, err
	}
	parameters := map[string]any{}
	if resolution != "" {
		mapped, ok := veoResolutions[resolution]
		if !ok {
			return nil, fmt.Errorf("Veo renders hd, fhd and uhd; %s is Black Forest Labs' step", resolution)
		}
		parameters["resolution"] = mapped
	}
	if aspect := strings.TrimSpace(req.AspectRatio); aspect != "" && aspect != "auto" {
		if !listHas(veoAspectRatios, aspect) {
			return nil, fmt.Errorf("Veo renders %s, not %q", strings.Join(veoAspectRatios, " and "), req.AspectRatio)
		}
		parameters["aspectRatio"] = aspect
	}
	if req.Duration != 0 {
		if !hasInt(veoDurations, req.Duration) {
			return nil, fmt.Errorf("Veo renders 4, 6 or 8 seconds, not %d", req.Duration)
		}
		// Une chaîne, parce que c'est ce que l'API attend là où on lirait un
		// nombre.
		parameters["durationSeconds"] = fmt.Sprintf("%d", req.Duration)
	}
	if req.Draft {
		return nil, fmt.Errorf("the draft mode is Black Forest Labs'; Veo has no cheaper pass")
	}

	instance := map[string]any{"prompt": req.Prompt}
	// La première image ouvre le plan, la seconde le ferme — Veo appelle la
	// seconde `lastFrame` et interpole entre les deux.
	if len(req.Keyframes) > 0 {
		instance["image"] = veoInline(req.Keyframes[0])
	}
	if len(req.Keyframes) > 1 {
		instance["lastFrame"] = veoInline(req.Keyframes[1])
	}

	payload := map[string]any{"instances": []any{instance}}
	if len(parameters) > 0 {
		payload["parameters"] = parameters
	}

	operation, err := c.veoSubmit(ctx, model, payload)
	if err != nil {
		return nil, err
	}
	uri, err := c.veoAwait(ctx, operation)
	if err != nil {
		return nil, err
	}
	return c.veoDownload(ctx, uri, req.Duration)
}

func veoInline(frame ImageResult) map[string]any {
	mime := frame.MimeType
	if mime == "" {
		mime = "image/png"
	}
	return map[string]any{
		"inlineData": map[string]string{
			"mimeType": mime,
			"data":     base64.StdEncoding.EncodeToString(frame.Data),
		},
	}
}

// checkVeoModel refuse un nom qui n'est pas un modèle Veo, ou qui contient de
// quoi sortir du chemin.
func checkVeoModel(model string) error {
	if !strings.HasPrefix(model, veoModelPrefix) {
		return fmt.Errorf("%q is not a Veo model (they are named veo-…)", model)
	}
	for _, r := range model {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
		default:
			return fmt.Errorf("%q is not a model name this engine will put in a url", model)
		}
	}
	return nil
}

func (c *Config) geminiBase() string {
	if c != nil && strings.TrimSpace(c.GeminiBaseURL) != "" {
		return strings.TrimSuffix(strings.TrimSpace(c.GeminiBaseURL), "/")
	}
	return defaultGeminiBaseURL
}

func (c *Config) veoSubmit(ctx context.Context, model string, payload map[string]any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:predictLongRunning", c.geminiBase(), model)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-goog-api-key", c.GoogleAPIKey)

	res, err := httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("video generation could not reach Google: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", normalizeHTTPError(res.StatusCode, raw)
	}
	var op veoOperation
	if err := json.Unmarshal(raw, &op); err != nil {
		return "", fmt.Errorf("Google returned something this engine cannot read: %w", err)
	}
	if strings.TrimSpace(op.Name) == "" {
		return "", fmt.Errorf("Google accepted the request but named no operation")
	}
	return op.Name, nil
}

// veoAwait interroge l'opération jusqu'à ce qu'elle se dise faite.
//
// Le délai vient du contexte de l'appelant : une exécution en a déjà un, et en
// inventer un second ici voudrait dire abandonner une vidéo pendant que le
// budget du workflow dit encore qu'on peut attendre.
func (c *Config) veoAwait(ctx context.Context, operation string) (string, error) {
	// Le nom vient de la réponse qu'on vient de recevoir, mais il devient un
	// chemin : on refuse ce qui le ferait sortir de l'hôte.
	if strings.Contains(operation, "://") || strings.Contains(operation, "..") {
		return "", fmt.Errorf("Google named an operation this engine will not follow")
	}
	url := fmt.Sprintf("%s/v1beta/%s", c.geminiBase(), strings.TrimPrefix(operation, "/"))
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("video generation gave up waiting for Google: %w", ctx.Err())
		case <-time.After(veoPollInterval):
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("x-goog-api-key", c.GoogleAPIKey)
		res, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("video generation lost contact with Google: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
		res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return "", normalizeHTTPError(res.StatusCode, raw)
		}
		var op veoOperation
		if err := json.Unmarshal(raw, &op); err != nil {
			return "", fmt.Errorf("Google returned something this engine cannot read: %w", err)
		}
		if !op.Done {
			continue
		}
		if op.Error != nil && strings.TrimSpace(op.Error.Message) != "" {
			return "", fmt.Errorf("Veo failed while generating: %s", op.Error.Message)
		}
		samples := op.Response.GenerateVideoResponse.GeneratedSamples
		if len(samples) == 0 || strings.TrimSpace(samples[0].Video.URI) == "" {
			// Le cas le plus fréquent derrière ça est un refus de sécurité : le
			// travail se termine, et rien ne sort.
			return "", fmt.Errorf("Veo finished without a video, which is what a refused prompt looks like")
		}
		return samples[0].Video.URI, nil
	}
}

// veoDownload va chercher le fichier. L'URI est servie par Google et demande la
// clé, ce qui en fait le seul téléchargement authentifié du moteur.
func (c *Config) veoDownload(ctx context.Context, uri string, seconds int) (*VideoResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-goog-api-key", c.GoogleAPIKey)
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the generated video could not be downloaded: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("the generated video could not be downloaded (%d)", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxVideoBytes))
	if err != nil {
		return nil, fmt.Errorf("the generated video could not be read: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the generated video came back empty")
	}
	mime := res.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if !strings.HasPrefix(mime, "video/") {
		// Veo rend du mp4 et l'annonce ; un autre type est une page d'erreur
		// déguisée en téléchargement, et la mettre dans un lecteur vidéo
		// n'apprendrait rien à personne.
		return nil, fmt.Errorf("Google sent %q where a video was expected", mime)
	}
	return &VideoResult{Data: data, MimeType: mime, Seconds: seconds}, nil
}

func hasInt(list []int, v int) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
