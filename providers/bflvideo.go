package providers

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FLUX 3 Video, chez Black Forest Labs.
//
// La même mécanique que leurs images — on dépose une demande, on reçoit une URL
// à interroger, on télécharge le résultat avant qu'il expire — et ce fichier ne
// réécrit ni la soumission ni l'attente : bflSubmit et bflAwait sont ceux de
// blackforestlabs.go. Ce qui est propre à la vidéo tient dans le corps de la
// requête et dans les valeurs que l'API accepte.
//
// La facturation est à la seconde et monte avec la résolution : le brouillon
// est à six centimes la seconde et n'existe qu'en HD, et la HD pleine coûte
// déjà plusieurs fois ça, l'UHD plus d'un ordre de grandeur. C'est pourquoi les
// refus sont écrits avant l'envoi : une demande mal formée qu'on laisserait
// partir coûterait une attente, puis un message du serveur, puis la seconde
// tentative.

// bflVideoModels est l'allow-list des modèles, pour la même raison que celle des
// images : le nom devient un segment d'URL et vient d'un graphe. Un nœud qui
// pourrait nommer le chemin pourrait nommer n'importe quelle route de l'hôte.
var bflVideoModels = map[string]bool{
	"flux-3-video": true,
}

// BFLDefaultVideoModel est ce qu'un nœud obtient quand il ne nomme rien.
const BFLDefaultVideoModel = "flux-3-video"

// bflVideoDurations borne ce que l'API accepte : de cinq à vingt secondes
// entières, ou « auto ».
const (
	bflMinDuration = 5
	bflMaxDuration = 20
)

// bflAspectRatios est ce que FLUX 3 Video accepte. La liste est là plutôt que
// dans un commentaire parce qu'elle sert à refuser, et un refus qui nomme les
// valeurs possibles est la moitié de la réparation.
var bflAspectRatios = []string{"auto", "21:9", "2:1", "16:9", "4:3", "1:1", "3:4", "9:16"}

func (c *Config) bflVideoGenerate(ctx context.Context, req VideoRequest) (*VideoResult, error) {
	if strings.TrimSpace(c.BFLAPIKey) == "" {
		return nil, fmt.Errorf("no Black Forest Labs key is configured")
	}
	slug := strings.ToLower(strings.TrimSpace(req.Model))
	if slug == "" {
		slug = BFLDefaultVideoModel
	}
	if !bflVideoModels[slug] {
		return nil, fmt.Errorf("%q is not a Black Forest Labs video model this engine calls", req.Model)
	}

	resolution, err := normalizeResolution(req.Resolution)
	if err != nil {
		return nil, err
	}
	// Le brouillon est la ligne la moins chère de la grille : rapide, HD
	// seulement. Demander de l'UHD en brouillon est une contradiction que le
	// serveur ne relèverait pas — il rendrait de la HD — donc elle se dit ici.
	if req.Draft && resolution != "" && resolution != "hd" {
		return nil, fmt.Errorf("the draft mode renders in hd only; ask for %s without draft", resolution)
	}

	aspect := strings.ToLower(strings.TrimSpace(req.AspectRatio))
	if aspect != "" && !listHas(bflAspectRatios, aspect) {
		return nil, fmt.Errorf("Black Forest Labs does not render %q (%s)", req.AspectRatio, strings.Join(bflAspectRatios, ", "))
	}

	body := map[string]any{
		"prompt": req.Prompt,
		"mode":   "t2v",
	}
	if resolution != "" {
		body["resolution"] = resolution
	}
	if aspect != "" {
		body["aspect_ratio"] = aspect
	}
	if req.Draft {
		body["draft"] = true
	}
	if req.Duration != 0 {
		if req.Duration < bflMinDuration || req.Duration > bflMaxDuration {
			return nil, fmt.Errorf("Black Forest Labs renders %d to %d seconds, not %d", bflMinDuration, bflMaxDuration, req.Duration)
		}
		body["duration"] = req.Duration
	}

	// Une image d'entrée fait du plan une animation : le mode change, et l'image
	// devient la première frame. Deux images épinglent le début et la fin.
	if len(req.Keyframes) > 0 {
		body["mode"] = "i2v"
		frames := make([]string, 0, 2)
		for i, frame := range req.Keyframes {
			if i >= 2 {
				break
			}
			frames = append(frames, base64.StdEncoding.EncodeToString(frame.Data))
		}
		if len(frames) == 1 {
			body["keyframes"] = frames[0]
		} else {
			body["keyframes"] = frames
		}
	}

	polling, err := c.bflSubmit(ctx, slug, body)
	if err != nil {
		return nil, err
	}
	sample, err := c.bflAwait(ctx, polling)
	if err != nil {
		return nil, err
	}
	return c.bflVideoDownload(ctx, sample, req.Duration)
}

// bflVideoDownload va chercher le fichier pendant qu'il existe encore.
//
// Séparé du téléchargement d'image pour une raison qui n'est pas cosmétique :
// une image qui arrive avec un type inattendu est traitée comme un PNG — le
// pari est sans risque. Une vidéo qui arrive avec un type inattendu est presque
// toujours une page d'erreur déguisée en téléchargement, et la nommer mp4
// mettrait quelques kilo-octets de HTML dans un lecteur vidéo.
func (c *Config) bflVideoDownload(ctx context.Context, sampleURL string, seconds int) (*VideoResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sampleURL, nil)
	if err != nil {
		return nil, err
	}
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
		return nil, fmt.Errorf("Black Forest Labs sent %q where a video was expected", mime)
	}
	return &VideoResult{Data: data, MimeType: mime, Seconds: seconds}, nil
}

// listHas : « cette valeur est-elle dans cette liste ». Nommée ainsi et pas
// `contains` parce qu'un test du paquet a déjà ce nom-là.
func listHas(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
