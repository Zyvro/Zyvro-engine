package engine

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// D'où vient une valeur, quand elle vient d'un fichier.
//
// Sans ça, un graphe ne peut pas nommer ce qu'il écrit d'après ce qu'il a lu :
// `fileInput` rend les octets et jette le chemin, et chaque nœud qui suit
// reconstruit sa sortie sans rien recopier de l'amont. On peut donc lire
// `sprites/53013.png`, le passer à un générateur d'image, et se retrouver à
// devoir écrire le résultat sous un nom écrit en dur. C'est ce qui bloque
// « un fichier → un fichier », et c'est un prérequis de « N fichiers →
// N fichiers ».
//
// **La provenance ne voyage pas dans la valeur.** C'était la première idée, et
// elle est fausse : il faudrait que chaque nœud — les bâtis, ceux d'un pack,
// ceux qu'on écrira l'an prochain — pense à recopier une clef de son entrée
// vers sa sortie. Un seul qui oublie, et le nom disparaît sans que rien ne le
// dise. La discipline qu'on ne peut pas vérifier n'est pas une solution.
//
// Elle se calcule donc **sur le graphe** : un nœud hérite de la provenance de
// ses amonts. Aucun nœud n'a à coopérer, y compris ceux d'un pack, et un nœud
// rejoué depuis le cache la retrouve comme les autres — elle ne dépend pas de
// l'exécution, seulement de la forme du graphe et de ce que les `fileInput` ont
// lu.
//
// **Deux sources qui remontent, c'est aucune.** Un nœud qui fusionne deux
// fichiers n'a pas « une » provenance ; en choisir une au hasard écrirait le
// résultat sous le nom d'une des deux entrées, une fois sur deux la mauvaise.
// Mieux vaut que `{{sourceName}}` refuse en le disant.

// sourceOf rend le chemin dont la valeur d'un nœud descend, ou "" quand il n'y
// en a pas, ou quand il y en a plusieurs.
func (r *Runtime) sourceOf(nodeID string) string {
	return r.sourceWalk(nodeID, map[string]string{}, map[string]bool{})
}

func (r *Runtime) sourceWalk(nodeID string, memo map[string]string, onPath map[string]bool) string {
	if found, ok := memo[nodeID]; ok {
		return found
	}
	// Un graphe est un DAG, mais on ne parie pas là-dessus : un cycle mal formé
	// ferait tourner ceci indéfiniment au lieu de rendre une erreur de graphe.
	if onPath[nodeID] {
		return ""
	}
	onPath[nodeID] = true
	defer delete(onPath, nodeID)

	if own := r.Sources[nodeID]; own != "" {
		memo[nodeID] = own
		return own
	}

	var seule string
	for _, up := range UpstreamOf(r.Graph, nodeID) {
		amont := r.sourceWalk(up, memo, onPath)
		if amont == "" {
			continue
		}
		if seule == "" {
			seule = amont
			continue
		}
		if seule != amont {
			// Deux sources différentes : la provenance n'est plus un chemin,
			// c'est une question. On rend "" et `{{sourceName}}` refusera en le
			// disant, plutôt que d'écrire sous un nom tiré au sort.
			memo[nodeID] = ""
			return ""
		}
	}
	memo[nodeID] = seule
	return seule
}

// rememberSource note ce qu'un nœud vient de lire. Appelé par `fileInput`, et
// par lui seul : c'est le seul nœud qui fasse entrer un chemin dans un graphe.
func (r *Runtime) rememberSource(nodeID, path string) {
	if nodeID == "" || path == "" {
		return
	}
	if r.Sources == nil {
		r.Sources = map[string]string{}
	}
	r.Sources[nodeID] = path
}

// Les motifs qu'un chemin de sortie peut porter.
//
// Écrits ici plutôt que dans le nœud parce que le message d'erreur en dépend :
// refuser en nommant les motifs possibles est la moitié de la réparation.
var sourcePlaceholder = regexp.MustCompile(`\{\{source([A-Za-z]*)\}\}`)

// resolveSource remplace les motifs de provenance dans un chemin de sortie.
//
// Il refuse plutôt que d'écrire un fichier au nom bizarre. C'est la leçon du
// fichier nommé `{{input:gfx}}` qui s'est retrouvé sur le disque de Jeremy : un
// motif qu'on ne sait pas remplacer et qu'on laisse passer devient un nom de
// fichier, et personne ne comprend d'où il sort. Le refus, lui, dit quoi faire.
func (r *Runtime) resolveSource(s, nodeID string) (string, error) {
	if !strings.Contains(s, "{{source") {
		return s, nil
	}
	src := r.sourceOf(nodeID)

	var bad error
	out := sourcePlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		champ := sourcePlaceholder.FindStringSubmatch(m)[1]
		if src == "" {
			if bad == nil {
				bad = fmt.Errorf(
					"%s needs to know which file the value came from, and nothing upstream read one "+
						"(or two different files reached this node): connect a file input, or write the name out in full",
					m)
			}
			return m
		}
		switch strings.ToLower(champ) {
		case "path":
			return src
		case "name":
			return path.Base(src)
		case "stem":
			return strings.TrimSuffix(path.Base(src), path.Ext(src))
		case "ext":
			return path.Ext(src)
		case "dir":
			return path.Dir(src)
		default:
			if bad == nil {
				bad = fmt.Errorf("%s is not a thing this engine knows: use {{sourcePath}}, {{sourceName}}, {{sourceStem}}, {{sourceExt}} or {{sourceDir}}", m)
			}
			return m
		}
	})
	if bad != nil {
		return "", bad
	}
	return out, nil
}

// SourcePlaceholderNames rend les motifs de provenance qu'un texte porte, sans
// les remplacer. La fenêtre s'en sert pour dire à l'avance qu'un graphe a
// besoin d'une entrée fichier — un refus au moment du run est un refus tardif.
func SourcePlaceholderNames(s string) []string {
	var out []string
	vus := map[string]bool{}
	for _, m := range sourcePlaceholder.FindAllString(s, -1) {
		if !vus[m] {
			vus[m] = true
			out = append(out, m)
		}
	}
	return out
}
