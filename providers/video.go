package providers

import (
	"context"
	"fmt"
	"strings"
)

// La vidéo, et les deux dos qui savent en faire.
//
// Même forme que les images, pour la même raison : le reste du moteur appelle
// VideoGenerate sans nommer de fournisseur, et le choix se fait ici — une fois,
// dans un fichier, au lieu d'une fois par appelant.
//
// Ce qui diffère des images, et qui a décidé la forme de ce fichier : une vidéo
// se demande, puis s'attend. Les deux API sont asynchrones, les deux rendent un
// lien qui expire, et les deux facturent à la seconde. Une requête mal formée
// coûte donc une minute d'attente avant de dire non — d'où les refus écrits ici,
// avant l'envoi, qui nomment ce que le dos accepte vraiment.

// VideoProviders est l'ensemble des fournisseurs qui peuvent servir un nœud
// vidéo. Comme pour les autres rôles, la liste vit là et le catalogue du démon
// la lit : une seconde copie serait celle qu'on oublierait le jour d'un
// troisième dos.
var VideoProviders = []string{"google", "bfl"}

// VideoResult est ce qui revient : les octets, et de quoi les nommer.
//
// Les octets, pas un lien : les deux fournisseurs rendent une URL qui expire —
// dix minutes chez Black Forest Labs — et un lien mort au milieu d'un workflow
// qui tourne encore n'est pas un résultat.
type VideoResult struct {
	Data     []byte
	MimeType string
	// Seconds est la durée demandée, telle que le dos l'a acceptée. Elle
	// remonte parce que c'est l'unité de facturation des deux : une vidéo dont
	// on ne sait pas la longueur est une facture qu'on ne sait pas lire.
	Seconds int
}

// VideoRequest est ce qu'un nœud demande.
//
// Une structure plutôt que huit paramètres : l'appel traverse le moteur, le
// sandbox et deux adaptateurs, et huit paramètres positionnels finissent par
// être intervertis par quelqu'un qui ajoute le neuvième.
type VideoRequest struct {
	Provider    string
	Model       string
	Prompt      string
	AspectRatio string
	// Resolution parle le vocabulaire du tableau de prix : hd, fhd, qhd, uhd.
	// Vide veut dire « celle du dos », qui est hd des deux côtés.
	Resolution string
	// Duration en secondes entières. Zéro veut dire « celle du dos » : auto
	// chez Black Forest Labs, huit secondes chez Veo.
	Duration int
	// Draft est la ligne à six centimes la seconde du tableau : une exploration
	// rapide, en HD, sans les autres résolutions. Elle n'existe que chez Black
	// Forest Labs.
	Draft bool
	// Keyframes sont les images d'entrée. La première ouvre le plan ; la
	// seconde, quand elle est là, le ferme.
	Keyframes []ImageResult
}

// maxVideoBytes borne ce qu'on accepte de télécharger.
//
// Vingt secondes en UHD pèsent quelques dizaines de méga-octets ; la borne est
// là pour le cas où l'autre bout rend autre chose que ce qu'il a annoncé, pas
// pour discipliner une vidéo légitime.
const maxVideoBytes = 256 << 20

// VideoResolutions est le vocabulaire commun, dans l'ordre du tableau de prix.
// L'éditeur le montre, les deux adaptateurs le traduisent.
var VideoResolutions = []string{"hd", "fhd", "qhd", "uhd"}

// resolveVideoProvider choisit le dos d'un appel : celui que le nœud nomme, puis
// celui du déploiement, puis la préférence du compte, puis ce qui a une clé.
//
// Les synonymes sont ceux des images, aux modèles près : quelqu'un qui écrit
// « veo » pense à Google, et quelqu'un qui écrit « flux » pense à Black Forest
// Labs. Leur refuser le nom qu'ils ont en tête pour leur faire écrire le nôtre
// ne protège rien.
func (c *Config) resolveVideoProvider(requested string) string {
	for _, p := range []string{requested, c.VideoProvider} {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "google", "gemini", "veo":
			return "google"
		case "bfl", "blackforestlabs", "flux":
			return "bfl"
		}
	}
	return c.PreferredOrDefault("video", c.hasVideoCredential, func() string {
		if strings.TrimSpace(c.GoogleAPIKey) == "" && strings.TrimSpace(c.BFLAPIKey) != "" {
			return "bfl"
		}
		return "google"
	})
}

func (c *Config) hasVideoCredential(provider string) bool {
	switch provider {
	case "google":
		return strings.TrimSpace(c.GoogleAPIKey) != ""
	case "bfl":
		return strings.TrimSpace(c.BFLAPIKey) != ""
	}
	return false
}

// ResolvedVideoProvider dit à qui l'appel irait, sans le faire.
//
// Le moteur en a besoin avant d'appeler : le modèle par défaut du déploiement
// est celui de Veo, et le donner à Black Forest Labs serait lui donner un nom
// que personne là-bas n'a jamais entendu.
func (c *Config) ResolvedVideoProvider(requested string) string {
	return c.resolveVideoProvider(requested)
}

// VideoGenerate rend une vidéo avec le dos que la requête ou le déploiement
// désigne.
func (c *Config) VideoGenerate(ctx context.Context, req VideoRequest) (*VideoResult, error) {
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("video generation needs a prompt")
	}
	switch c.resolveVideoProvider(req.Provider) {
	case "bfl":
		return c.bflVideoGenerate(ctx, req)
	default:
		return c.geminiVideoGenerate(ctx, req)
	}
}

// normalizeResolution ramène ce qu'un nœud a écrit au vocabulaire commun, et
// refuse le reste en le nommant.
//
// Refuser plutôt que retomber sur hd : une vidéo en HD quand on a demandé de
// l'UHD est une facture juste et une image fausse, et personne ne saurait
// pourquoi. Les fautes de frappe sont acceptées quand elles ne sont pas
// ambiguës — « 1080p » et « fullhd » veulent dire fhd.
func normalizeResolution(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return "", nil
	case "hd", "720", "720p":
		return "hd", nil
	case "fhd", "fullhd", "1080", "1080p":
		return "fhd", nil
	case "qhd", "1440", "1440p", "2k":
		return "qhd", nil
	case "uhd", "4k", "2160", "2160p":
		return "uhd", nil
	}
	return "", fmt.Errorf("%q is not a video resolution this engine knows (hd, fhd, qhd, uhd)", v)
}
