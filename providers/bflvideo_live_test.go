package providers

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// L'essai contre la vraie API de Black Forest Labs.
//
// Tous les autres tests de ce fichier parlent à un serveur de boucle locale :
// ils vérifient ce qui part, ce qui revient et ce qui doit être refusé avant
// l'envoi, et ils sont gratuits. Celui-ci parle au vrai serveur, et une vidéo
// se facture à la seconde — d'où la porte fermée.
//
// Il ne part que si on lui donne une clef dans l'environnement :
//
//	ZYVRO_LIVE_BFL_KEY=… go test ./providers -run TestBFLVideoLive -v
//
// `go test ./...` le saute, y compris sur une machine où la clef traîne dans un
// projet : une dépense ne doit pas pouvoir arriver parce qu'on a lancé la suite.
//
// Ce qu'il demande est la ligne la moins chère de la grille et rien d'autre :
// brouillon, HD, cinq secondes — le minimum que l'API accepte. Le reste de la
// grille n'a pas besoin d'être essayé pour être cru : c'est le même appel avec
// d'autres valeurs, et ce sont justement les valeurs qui coûtent.
func TestBFLVideoLive(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("ZYVRO_LIVE_BFL_KEY"))
	if key == "" {
		t.Skip("no ZYVRO_LIVE_BFL_KEY: this test spends real money and stays shut without one")
	}

	cfg := &Config{BFLAPIKey: key}
	// Large : une génération vidéo n'est pas une requête, c'est une file
	// d'attente. Trop court, et on paie un rendu qu'on n'attend pas.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	started := time.Now()
	out, err := cfg.bflVideoGenerate(ctx, VideoRequest{
		Model:       BFLDefaultVideoModel,
		Prompt:      "a single red balloon rising slowly against a plain grey sky",
		Resolution:  "hd",
		AspectRatio: "16:9",
		Duration:    bflMinDuration,
		Draft:       true,
	})
	if err != nil {
		t.Fatalf("the live call failed: %v", err)
	}
	if len(out.Data) == 0 {
		t.Fatal("the call succeeded and brought back nothing")
	}
	// Une page d'erreur déguisée en téléchargement pèse quelques kilo-octets et
	// se laisse nommer mp4 : c'est le cas que bflVideoDownload refuse, et le
	// vérifier ici est la seule façon de savoir qu'il l'a fait pour de vrai.
	if !strings.HasPrefix(out.MimeType, "video/") {
		t.Fatalf("what came back is not a video: %s (%d bytes)", out.MimeType, len(out.Data))
	}
	// La durée remonte parce que c'est l'unité de facturation : une vidéo dont
	// on ne sait pas la longueur est une facture qu'on ne sait pas lire.
	if out.Seconds != bflMinDuration {
		t.Fatalf("asked for %d seconds and got %d back", bflMinDuration, out.Seconds)
	}
	t.Logf("live: %d bytes, %s, %d s, in %s", len(out.Data), out.MimeType, out.Seconds, time.Since(started).Round(time.Second))
}
