package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// La vidéo, sur des serveurs de boucle locale.
//
// Ce qui casse en silence ici, et que ces tests tiennent :
//
//  1. **Une demande que le dos ne sait pas faire.** Les deux facturent à la
//     seconde et rendent en dizaines de secondes : une résolution ou une durée
//     qu'ils refusent doit se dire avant l'envoi, pas après une attente payée.
//
//  2. **Un vocabulaire traduit de travers.** Le nœud parle hd/fhd/qhd/uhd. Veo
//     parle 720p/1080p/4k et n'a pas de QHD ; ramener silencieusement la
//     demande à l'étage du dessous rendrait une vidéo plus petite que demandé
//     sans que rien ne le dise.
//
//  3. **Le choix du dos.** Le même graphe doit tourner chez qui a une clé
//     Google et chez qui a une clé Black Forest Labs, sans que personne
//     l'édite.
//
//  4. **Ce qui revient.** Les deux rendent un lien qui expire ; ce qui doit
//     sortir d'ici, ce sont des octets, et refuser une page d'erreur déguisée
//     en téléchargement.

// Une attente de dix secondes est ce que Google recommande en production et
// ce qu'on ne paie pas ici : les tests vérifient la mécanique, pas la patience.
func quickPolling(t *testing.T) {
	t.Helper()
	was := veoPollInterval
	veoPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { veoPollInterval = was })
}

func TestVideoResolutionVocabulary(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"hd", "hd"},
		{"HD", "hd"},
		{"720p", "hd"},
		{"1080p", "fhd"},
		{"fullhd", "fhd"},
		{"qhd", "qhd"},
		{"4k", "uhd"},
		{" uhd ", "uhd"},
	} {
		got, err := normalizeResolution(c.in)
		if err != nil || got != c.want {
			t.Fatalf("normalizeResolution(%q) = %q, %v — voulu %q", c.in, got, err, c.want)
		}
	}
	if _, err := normalizeResolution("8k"); err == nil {
		t.Fatal("une résolution inconnue doit être refusée plutôt que ramenée à hd")
	}
}

// ---- le choix du dos ----------------------------------------------------

func TestVideoProviderFollowsTheCredential(t *testing.T) {
	google := &Config{GoogleAPIKey: "g"}
	if got := google.ResolvedVideoProvider(""); got != "google" {
		t.Fatalf("une clé Google seule doit router vers Google, pas %q", got)
	}
	bfl := &Config{BFLAPIKey: "b"}
	if got := bfl.ResolvedVideoProvider(""); got != "bfl" {
		t.Fatalf("une clé Black Forest Labs seule doit y router, pas %q", got)
	}
	both := &Config{GoogleAPIKey: "g", BFLAPIKey: "b"}
	if got := both.ResolvedVideoProvider("flux"); got != "bfl" {
		t.Fatalf("le nœud décide en premier, pas %q", got)
	}
	if got := both.ResolvedVideoProvider("veo"); got != "google" {
		t.Fatalf("« veo » est un nom que quelqu'un écrira, pas %q", got)
	}
	// La préférence du compte passe avant la règle écrite dans le code.
	preferred := &Config{GoogleAPIKey: "g", BFLAPIKey: "b", Preference: Preference{"video": {"bfl"}}}
	if got := preferred.ResolvedVideoProvider(""); got != "bfl" {
		t.Fatalf("la préférence du compte doit décider, pas %q", got)
	}
	if ProvidersFor("video") == nil {
		t.Fatal("le catalogue doit savoir qui peut faire de la vidéo")
	}
}

func TestVideoNeedsAPrompt(t *testing.T) {
	c := &Config{GoogleAPIKey: "g"}
	if _, err := c.VideoGenerate(context.Background(), VideoRequest{Prompt: "  "}); err == nil {
		t.Fatal("une vidéo sans description doit être refusée avant l'envoi")
	}
}

// ---- Black Forest Labs --------------------------------------------------

// bflVideoServer rejoue la mécanique du vendeur : on dépose, on interroge, on
// télécharge.
type bflVideoServer struct {
	*httptest.Server
	path string
	body map[string]any
	mime string
}

func newBFLVideoServer(t *testing.T) *bflVideoServer {
	t.Helper()
	s := &bflVideoServer{mime: "video/mp4"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		s.path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&s.body)
		fmt.Fprintf(w, `{"id":"t","polling_url":%q}`, s.URL+"/result")
	})
	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"t","status":"Ready","result":{"sample":%q}}`, s.URL+"/delivery.mp4")
	})
	mux.HandleFunc("/delivery.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", s.mime)
		fmt.Fprint(w, "des octets de vidéo")
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestBFLVideoSendsWhatTheNodeAsked(t *testing.T) {
	s := newBFLVideoServer(t)
	c := &Config{BFLAPIKey: "k", BFLBaseURL: s.URL}

	out, err := c.VideoGenerate(context.Background(), VideoRequest{
		Provider:    "bfl",
		Prompt:      "une mouette au-dessus du port",
		Resolution:  "fhd",
		Duration:    8,
		AspectRatio: "16:9",
	})
	if err != nil {
		t.Fatalf("génération : %v", err)
	}
	if string(out.Data) != "des octets de vidéo" || out.MimeType != "video/mp4" {
		t.Fatalf("ce sont les octets qui doivent revenir, pas un lien : %q %q", out.Data, out.MimeType)
	}
	if out.Seconds != 8 {
		t.Fatalf("la durée revient avec le résultat : %d", out.Seconds)
	}
	if s.path != "/v1/flux-3-video" {
		t.Fatalf("le modèle par défaut décide de la route : %q", s.path)
	}
	if s.body["resolution"] != "fhd" || s.body["aspect_ratio"] != "16:9" {
		t.Fatalf("la résolution et le format partent tels quels : %v", s.body)
	}
	if s.body["duration"] != float64(8) {
		t.Fatalf("la durée part en secondes : %v", s.body["duration"])
	}
	if s.body["mode"] != "t2v" {
		t.Fatalf("sans image, c'est du texte vers vidéo : %v", s.body["mode"])
	}
}

func TestBFLVideoWithAnImageAnimatesIt(t *testing.T) {
	s := newBFLVideoServer(t)
	c := &Config{BFLAPIKey: "k", BFLBaseURL: s.URL}
	_, err := c.VideoGenerate(context.Background(), VideoRequest{
		Provider:  "bfl",
		Prompt:    "elle décolle",
		Keyframes: []ImageResult{{Data: []byte("png"), MimeType: "image/png"}},
	})
	if err != nil {
		t.Fatalf("génération : %v", err)
	}
	if s.body["mode"] != "i2v" {
		t.Fatalf("une image d'entrée fait du plan une animation : %v", s.body["mode"])
	}
	if _, ok := s.body["keyframes"].(string); !ok {
		t.Fatalf("une seule image part seule, pas dans une liste : %T", s.body["keyframes"])
	}
}

func TestBFLVideoRefusesWhatItCannotDo(t *testing.T) {
	s := newBFLVideoServer(t)
	c := &Config{BFLAPIKey: "k", BFLBaseURL: s.URL}
	ctx := context.Background()

	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "bfl", Prompt: "p", Duration: 45}); err == nil {
		t.Fatal("quarante-cinq secondes doivent être refusées ici, pas facturées là-bas")
	}
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "bfl", Prompt: "p", Duration: 2}); err == nil {
		t.Fatal("deux secondes sont sous le minimum")
	}
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "bfl", Prompt: "p", AspectRatio: "7:3"}); err == nil {
		t.Fatal("un format inconnu doit être nommé avant l'envoi")
	}
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "bfl", Prompt: "p", Model: "flux-9-video"}); err == nil {
		t.Fatal("un modèle hors liste devient un chemin : il doit être refusé")
	}
	// Le brouillon est la ligne à six centimes, et elle n'existe qu'en HD.
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "bfl", Prompt: "p", Draft: true, Resolution: "uhd"}); err == nil {
		t.Fatal("un brouillon en UHD est une contradiction que le serveur ne relèverait pas")
	}
	if s.path != "" {
		t.Fatalf("aucune de ces demandes ne doit être partie : %q", s.path)
	}
}

func TestBFLVideoDraftGoesOut(t *testing.T) {
	s := newBFLVideoServer(t)
	c := &Config{BFLAPIKey: "k", BFLBaseURL: s.URL}
	if _, err := c.VideoGenerate(context.Background(), VideoRequest{Provider: "bfl", Prompt: "p", Draft: true}); err != nil {
		t.Fatalf("génération : %v", err)
	}
	if s.body["draft"] != true {
		t.Fatalf("le brouillon doit voyager : %v", s.body)
	}
}

func TestBFLVideoRefusesAnErrorPageDressedAsAVideo(t *testing.T) {
	s := newBFLVideoServer(t)
	s.mime = "text/html"
	c := &Config{BFLAPIKey: "k", BFLBaseURL: s.URL}
	_, err := c.VideoGenerate(context.Background(), VideoRequest{Provider: "bfl", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "text/html") {
		t.Fatalf("du HTML dans un lecteur vidéo n'apprend rien à personne : %v", err)
	}
}

// ---- Veo ----------------------------------------------------------------

type veoServer struct {
	*httptest.Server
	path        string
	body        map[string]any
	key         string
	downloaded  bool
	downloadKey string
	mime        string
}

func newVeoServer(t *testing.T) *veoServer {
	t.Helper()
	s := &veoServer{mime: "video/mp4"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1beta/models/", func(w http.ResponseWriter, r *http.Request) {
		s.path = r.URL.Path
		s.key = r.Header.Get("x-goog-api-key")
		_ = json.NewDecoder(r.Body).Decode(&s.body)
		fmt.Fprint(w, `{"name":"models/veo/operations/abc"}`)
	})
	mux.HandleFunc("/v1beta/models/veo/operations/abc", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"name":"models/veo/operations/abc","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":%q}}]}}}`, s.URL+"/files/out.mp4")
	})
	mux.HandleFunc("/files/out.mp4", func(w http.ResponseWriter, r *http.Request) {
		s.downloaded = true
		s.downloadKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", s.mime)
		fmt.Fprint(w, "une vidéo de Veo")
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestVeoSubmitsPollsAndDownloads(t *testing.T) {
	quickPolling(t)
	s := newVeoServer(t)
	c := &Config{GoogleAPIKey: "g", GeminiBaseURL: s.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := c.VideoGenerate(ctx, VideoRequest{
		Provider:    "google",
		Prompt:      "un phare dans la brume",
		Resolution:  "fhd",
		Duration:    8,
		AspectRatio: "16:9",
	})
	if err != nil {
		t.Fatalf("génération : %v", err)
	}
	if string(out.Data) != "une vidéo de Veo" {
		t.Fatalf("ce sont les octets qui reviennent : %q", out.Data)
	}
	if !strings.HasSuffix(s.path, ":predictLongRunning") {
		t.Fatalf("la vidéo ne passe pas par generateContent : %q", s.path)
	}
	if !strings.Contains(s.path, veoDefaultModel) {
		t.Fatalf("le modèle par défaut doit être dans la route : %q", s.path)
	}
	if s.key == "" {
		t.Fatal("la clé voyage en en-tête")
	}
	params, _ := s.body["parameters"].(map[string]any)
	if params["resolution"] != "1080p" {
		t.Fatalf("fhd se dit 1080p chez Veo : %v", params)
	}
	if params["durationSeconds"] != "8" {
		t.Fatalf("la durée part en chaîne, comme l'API l'attend : %v", params["durationSeconds"])
	}
	if !s.downloaded || s.downloadKey == "" {
		t.Fatal("le fichier de Veo se télécharge avec la clé : c'est le seul du moteur")
	}
}

func TestVeoRefusesWhatItCannotDo(t *testing.T) {
	quickPolling(t)
	s := newVeoServer(t)
	c := &Config{GoogleAPIKey: "g", GeminiBaseURL: s.URL}
	ctx := context.Background()

	// Le piège de la traduction : QHD n'existe pas chez Veo, et le ramener à
	// 1080p rendrait une vidéo plus petite que demandé sans le dire.
	_, err := c.VideoGenerate(ctx, VideoRequest{Provider: "google", Prompt: "p", Resolution: "qhd"})
	if err == nil || !strings.Contains(err.Error(), "qhd") {
		t.Fatalf("QHD doit être refusé en le nommant : %v", err)
	}
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "google", Prompt: "p", Duration: 5}); err == nil {
		t.Fatal("Veo rend 4, 6 ou 8 secondes — cinq doit être refusé")
	}
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "google", Prompt: "p", AspectRatio: "1:1"}); err == nil {
		t.Fatal("Veo ne rend que 16:9 et 9:16")
	}
	if _, err := c.VideoGenerate(ctx, VideoRequest{Provider: "google", Prompt: "p", Draft: true}); err == nil {
		t.Fatal("le brouillon est celui de Black Forest Labs")
	}
	if s.path != "" {
		t.Fatalf("aucune de ces demandes ne doit être partie : %q", s.path)
	}
}

func TestVeoModelNameCannotNameAnotherRoute(t *testing.T) {
	if err := checkVeoModel("veo-3.1-generate-preview"); err != nil {
		t.Fatalf("un vrai nom doit passer : %v", err)
	}
	for _, bad := range []string{"gemini-2.5-flash", "veo-3/../../files", "veo-3:generateContent", "veo-3?key=x"} {
		if err := checkVeoModel(bad); err == nil {
			t.Fatalf("%q devient un segment d'url : il doit être refusé", bad)
		}
	}
}

func TestVeoSaysSoWhenNothingComesBack(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1beta/models/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"models/veo/operations/abc"}`)
	})
	mux.HandleFunc("/v1beta/models/veo/operations/abc", func(w http.ResponseWriter, r *http.Request) {
		// Un travail qui se termine sans échantillon : c'est à quoi ressemble
		// un prompt refusé.
		fmt.Fprint(w, `{"name":"models/veo/operations/abc","done":true,"response":{}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	quickPolling(t)

	c := &Config{GoogleAPIKey: "g", GeminiBaseURL: srv.URL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := c.VideoGenerate(ctx, VideoRequest{Provider: "google", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("une fin sans vidéo doit nommer la cause la plus probable : %v", err)
	}
}
