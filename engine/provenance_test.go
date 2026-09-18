package engine

import (
	"context"
	"strings"
	"testing"
)

// D'où vient une valeur, et ce qu'un graphe peut en faire.
//
// Ce qui casse en silence ici :
//
//  1. **Un nom perdu en route.** La provenance traverse des nœuds qui ne savent
//     rien d'elle. Si elle voyageait dans la valeur, il suffirait qu'un seul
//     nœud oublie de la recopier pour que le nom disparaisse — sans erreur,
//     sans trace, et le fichier sortirait sous le nom écrit en dur.
//
//  2. **Un nom tiré au sort.** Deux fichiers qui remontent jusqu'au même nœud,
//     ce n'est pas « une » provenance. En choisir une écrirait le résultat sous
//     le nom d'une des deux entrées, une fois sur deux la mauvaise.
//
//  3. **Un motif qui devient un nom de fichier.** Déjà vu sur le disque de
//     Jeremy : `abyssal-hd/{{input:gfx}}`. Un motif qu'on ne sait pas remplacer
//     et qu'on laisse passer devient un nom que personne ne comprend.

// runGraph exécute un graphe entier avec un dossier de projet en mémoire, comme
// le fait une vraie exécution : c'est la boucle du DAG qui décide de l'ordre, et
// c'est elle qu'on veut éprouver, pas un appel de fonction isolé.
func runGraph(t *testing.T, g *Graph, files *fakeFiles) (*Runtime, error) {
	t.Helper()
	rt := &Runtime{
		ExecID:  "t",
		Graph:   g,
		Outputs: map[string]*NodeOutput{},
		Files:   files,
	}
	return rt, rt.Execute(context.Background())
}

func filesWith(entries map[string]string) *fakeFiles {
	f := newFakeFiles()
	for k, v := range entries {
		f.read[k] = []byte(v)
		f.mimes[k] = "text/plain"
	}
	return f
}

// Le cas qui débloque « un fichier → un fichier » : lire, et écrire sous un nom
// tiré de ce qu'on a lu.
func TestSourceNamesTheOutput(t *testing.T) {
	for _, tc := range []struct {
		motif string
		veut  string
	}{
		{"out/{{sourceName}}", "out/53013.png"},
		{"out/{{sourceStem}}-hd{{sourceExt}}", "out/53013-hd.png"},
		{"{{sourceDir}}-hd/{{sourceName}}", "sprites/abyssal-hd/53013.png"},
		{"copie/{{sourcePath}}", "copie/sprites/abyssal/53013.png"},
	} {
		t.Run(tc.motif, func(t *testing.T) {
			files := filesWith(map[string]string{"sprites/abyssal/53013.png": "des octets"})
			g := &Graph{
				Nodes: []GraphNode{
					node("A", "fileInput", map[string]any{"path": "sprites/abyssal/53013.png", "as": "text"}),
					node("B", "fileOutput", map[string]any{"path": tc.motif}),
				},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")},
			}
			if _, err := runGraph(t, g, files); err != nil {
				t.Fatalf("le graphe a échoué : %v", err)
			}
			if _, ok := files.wrote[tc.veut]; !ok {
				t.Fatalf("écrit %v, attendu %s", keysOf(files.wrote), tc.veut)
			}
		})
	}
}

// La provenance doit traverser ce qui ne la connaît pas : c'est tout l'intérêt
// de la calculer sur le graphe plutôt que de la faire recopier par les nœuds.
func TestSourceSurvivesNodesThatKnowNothingAboutIt(t *testing.T) {
	files := filesWith(map[string]string{"notes/lettre.txt": "bonjour"})
	g := &Graph{
		Nodes: []GraphNode{
			node("A", "fileInput", map[string]any{"path": "notes/lettre.txt"}),
			node("B", "mergeText", map[string]any{"separator": " "}),
			node("C", "preview", nil),
			node("D", "fileOutput", map[string]any{"path": "sorties/{{sourceStem}}.md"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "B", "C"), dataEdge("e3", "C", "D")},
	}
	if _, err := runGraph(t, g, files); err != nil {
		t.Fatalf("le graphe a échoué : %v", err)
	}
	if _, ok := files.wrote["sorties/lettre.md"]; !ok {
		t.Fatalf("écrit %v, attendu sorties/lettre.md", keysOf(files.wrote))
	}
}

// Deux fichiers qui remontent : la provenance est une question, pas un chemin.
func TestTwoSourcesAreNoSource(t *testing.T) {
	files := filesWith(map[string]string{"a.txt": "un", "b.txt": "deux"})
	g := &Graph{
		Nodes: []GraphNode{
			node("A", "fileInput", map[string]any{"path": "a.txt"}),
			node("B", "fileInput", map[string]any{"path": "b.txt"}),
			node("C", "mergeText", map[string]any{"separator": " "}),
			node("D", "fileOutput", map[string]any{"path": "out/{{sourceName}}"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "C"), dataEdge("e2", "B", "C"), dataEdge("e3", "C", "D")},
	}
	_, err := runGraph(t, g, files)
	if err == nil {
		t.Fatalf("deux sources ont produit un nom : %v", keysOf(files.wrote))
	}
	if !strings.Contains(err.Error(), "two different files") {
		t.Fatalf("le refus n'explique pas l'ambiguïté : %v", err)
	}
	if len(files.wrote) != 0 {
		t.Fatalf("un fichier a été écrit malgré le refus : %v", keysOf(files.wrote))
	}
}

// Le même refus quand rien n'a lu de fichier : sans ça, `{{sourceName}}`
// deviendrait un nom de fichier sur le disque.
func TestSourceWithoutAFileRefuses(t *testing.T) {
	files := newFakeFiles()
	g := &Graph{
		Nodes: []GraphNode{
			node("A", "textInput", map[string]any{"value": "écrit à la main"}),
			node("B", "fileOutput", map[string]any{"path": "out/{{sourceName}}"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B")},
	}
	_, err := runGraph(t, g, files)
	if err == nil {
		t.Fatal("un motif sans source a été accepté")
	}
	if !strings.Contains(err.Error(), "{{sourceName}}") {
		t.Fatalf("le refus ne nomme pas le motif fautif : %v", err)
	}
	if len(files.wrote) != 0 {
		t.Fatalf("un fichier a été écrit : %v", keysOf(files.wrote))
	}
}

// Un motif mal orthographié est refusé en nommant ceux qui existent : un refus
// qui liste les valeurs possibles est la moitié de la réparation.
func TestUnknownSourcePlaceholderIsNamed(t *testing.T) {
	files := filesWith(map[string]string{"a.txt": "un"})
	g := &Graph{
		Nodes: []GraphNode{
			node("A", "fileInput", map[string]any{"path": "a.txt"}),
			node("B", "fileOutput", map[string]any{"path": "out/{{sourceFilename}}"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B")},
	}
	_, err := runGraph(t, g, files)
	if err == nil {
		t.Fatal("un motif inconnu est passé")
	}
	for _, veut := range []string{"{{sourceFilename}}", "{{sourceStem}}"} {
		if !strings.Contains(err.Error(), veut) {
			t.Fatalf("le refus ne dit pas %s : %v", veut, err)
		}
	}
}

// Un chemin sans motif ne doit pas changer, et surtout ne doit pas exiger une
// source : la grande majorité des graphes n'en veulent pas.
func TestAPathWithoutPlaceholdersNeedsNoSource(t *testing.T) {
	files := newFakeFiles()
	g := &Graph{
		Nodes: []GraphNode{
			node("A", "textInput", map[string]any{"value": "contenu"}),
			node("B", "fileOutput", map[string]any{"path": "out/fixe.txt"}),
		},
		Edges: []GraphEdge{dataEdge("e1", "A", "B")},
	}
	if _, err := runGraph(t, g, files); err != nil {
		t.Fatalf("un chemin fixe a été refusé : %v", err)
	}
	if _, ok := files.wrote["out/fixe.txt"]; !ok {
		t.Fatalf("écrit %v", keysOf(files.wrote))
	}
}

// La provenance se lit sur le graphe, sans l'exécuter. C'est ce qui fait qu'un
// nœud rejoué depuis le cache la garde : elle ne dépend pas de ce qui s'est
// passé, seulement de la forme du graphe et de ce que les entrées ont lu.
func TestSourceIsReadFromTheGraph(t *testing.T) {
	rt := &Runtime{
		Graph: &Graph{
			Nodes: []GraphNode{node("A", "fileInput", nil), node("B", "preview", nil), node("C", "preview", nil)},
			Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "B", "C")},
		},
		Sources: map[string]string{"A": "dossier/fichier.png"},
	}
	if got := rt.sourceOf("C"); got != "dossier/fichier.png" {
		t.Fatalf("la provenance ne descend pas : %q", got)
	}
	if got := rt.sourceOf("A"); got != "dossier/fichier.png" {
		t.Fatalf("le nœud qui lit ne connaît pas la sienne : %q", got)
	}
}

// Une arête qui remonte ne doit pas faire tourner la recherche indéfiniment :
// un graphe mal formé est une erreur de graphe, pas un test qui ne finit jamais.
func TestSourceSurvivesACycle(t *testing.T) {
	rt := &Runtime{
		Graph: &Graph{
			Nodes: []GraphNode{node("A", "preview", nil), node("B", "preview", nil)},
			Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "B", "A")},
		},
		Sources: map[string]string{},
	}
	if got := rt.sourceOf("A"); got != "" {
		t.Fatalf("un cycle a produit une provenance : %q", got)
	}
}

// Ce que la fenêtre peut demander avant de lancer, pour ne pas refuser tard.
func TestSourcePlaceholderNames(t *testing.T) {
	got := SourcePlaceholderNames("out/{{sourceStem}}-{{sourceExt}}/{{sourceStem}}.png")
	if len(got) != 2 || got[0] != "{{sourceStem}}" || got[1] != "{{sourceExt}}" {
		t.Fatalf("motifs relevés : %v", got)
	}
	if n := SourcePlaceholderNames("out/fixe.png"); len(n) != 0 {
		t.Fatalf("un chemin fixe porte des motifs : %v", n)
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
