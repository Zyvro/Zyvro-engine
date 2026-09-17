package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// Le nœud vidéo, de bout en bout.
//
// Il a refusé de tourner tant qu'aucun dos ne savait faire une vidéo, et ce que
// ces tests tiennent, c'est ce qui a changé ce jour-là :
//
//  1. **Le fichier ne voyage pas dans le graphe.** Une image générée circule en
//     data URL ; une vidéo de vingt secondes pèse des dizaines de méga-octets, et
//     la même habitude la mettrait dans le cache, dans la réponse de l'API et
//     dans le fichier du workflow. La sortie porte une adresse, pas les octets.
//
//  2. **Sans magasin, on le dit.** C'est le seul nœud qui en exige un, et un
//     refus qui nomme la cause vaut mieux qu'une vidéo qu'on ne peut pas ouvrir.
//
//  3. **Le prix suit ce que le nœud demande.** Résolution et durée sont les deux
//     réglages qui décident de la facture ; ils doivent arriver chez le dos tels
//     qu'ils ont été écrits.

// fakeBFLVideo rejoue les trois temps du vendeur — dépôt, attente, livraison —
// et retient ce qu'on lui a demandé.
type fakeBFLVideo struct {
	*httptest.Server
	body map[string]any
}

func newFakeBFLVideo(t *testing.T) *fakeBFLVideo {
	t.Helper()
	f := &fakeBFLVideo{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&f.body)
		fmt.Fprintf(w, `{"id":"t","polling_url":%q}`, f.URL+"/result")
	})
	mux.HandleFunc("/result", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"id":"t","status":"Ready","result":{"sample":%q}}`, f.URL+"/clip.mp4")
	})
	mux.HandleFunc("/clip.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		fmt.Fprint(w, "mp4-bytes")
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func videoGraph(cfg map[string]any) *Graph {
	return &Graph{
		Nodes: []GraphNode{
			node("A", "textInput", map[string]any{"value": "un train qui entre en gare"}),
			node("B", "generateVideo", cfg),
			node("C", "preview", nil),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "B", "C")},
	}
}

func TestGenerateVideoStoresTheFileAndPassesOnItsAddress(t *testing.T) {
	f := newFakeBFLVideo(t)
	store := &recordingStore{}
	cfg := &providers.Config{
		BFLAPIKey:  "k",
		BFLBaseURL: f.URL,
		// Une base Gemini qui échouerait si quoi que ce soit l'atteignait : un
		// appel qui partirait chez le mauvais dos doit être bruyant.
		GeminiBaseURL: "http://127.0.0.1:9",
	}
	g := videoGraph(map[string]any{"provider": "bfl", "resolution": "fhd", "duration": 8, "aspectRatio": "16:9"})
	rt := NewRuntime("exec-video", g, cfg, store, nil)
	if err := rt.Execute(context.Background()); err != nil {
		t.Fatalf("exécution : %v", err)
	}

	out := rt.Outputs["B"]
	if out == nil || out.Type != "video" {
		t.Fatalf("la sortie doit être une vidéo : %+v", out)
	}
	if url, _ := out.Value["url"].(string); !strings.HasSuffix(url, "video.mp4") {
		t.Fatalf("la sortie porte l'adresse du fichier écrit : %v", out.Value)
	}
	if _, present := out.Value["dataUrl"]; present {
		t.Fatal("une vidéo ne voyage pas en base64 dans le graphe")
	}
	if out.Value["seconds"] != 8 {
		t.Fatalf("la durée facturée remonte avec le résultat : %v", out.Value["seconds"])
	}
	if len(store.saved) != 1 || !strings.Contains(store.saved[0], "video/mp4") {
		t.Fatalf("le fichier doit être écrit une fois, en mp4 : %v", store.saved)
	}

	// Les deux réglages qui décident de la facture arrivent tels quels.
	if f.body["resolution"] != "fhd" || f.body["duration"] != float64(8) {
		t.Fatalf("la résolution et la durée doivent arriver telles qu'écrites : %v", f.body)
	}
	// Le prompt vient de l'amont quand le réglage est vide, comme pour l'image.
	if prompt, _ := f.body["prompt"].(string); !strings.Contains(prompt, "train") {
		t.Fatalf("le texte en amont devient la description : %v", f.body["prompt"])
	}
	// Et la sortie traverse le preview inchangée.
	if rt.Outputs["C"] != out {
		t.Fatal("le preview rend ce qu'il a reçu")
	}
}

func TestGenerateVideoNeedsSomewhereToPutTheFile(t *testing.T) {
	f := newFakeBFLVideo(t)
	cfg := &providers.Config{BFLAPIKey: "k", BFLBaseURL: f.URL}
	rt := NewRuntime("exec-video", videoGraph(map[string]any{"provider": "bfl"}), cfg, nil, nil)
	err := rt.Execute(context.Background())
	if err == nil || !strings.Contains(err.Error(), "media store") {
		t.Fatalf("sans magasin, il faut le dire plutôt que rendre une vidéo introuvable : %v", err)
	}
	if f.body != nil {
		t.Fatal("et il ne faut pas payer la génération avant de s'en apercevoir")
	}
}

func TestGenerateVideoIsInThePalette(t *testing.T) {
	var found *NodeKind
	for _, k := range Catalogue(nil) {
		if k.Type == "generateVideo" {
			kind := k
			found = &kind
		}
	}
	if found == nil {
		t.Fatal("le nœud a cessé d'être désactivé : il doit être proposable")
	}
	if len(found.Outputs) != 1 || found.Outputs[0] != "video" {
		t.Fatalf("il sort une vidéo : %v", found.Outputs)
	}
	// Les réglages qui décident du prix doivent être sur le nœud, pas cachés
	// dans un défaut de déploiement.
	for _, key := range []string{"resolution", "duration", "draft", "provider"} {
		if _, ok := found.Defaults[key]; !ok {
			t.Errorf("le réglage %q doit être sur le nœud : %v", key, found.Defaults)
		}
	}
}
