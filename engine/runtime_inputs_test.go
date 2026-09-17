package engine

import "testing"

func inputNode(id, typ, key, label string, cfg map[string]any) GraphNode {
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["inputKey"] = key
	return GraphNode{ID: id, Type: typ, Data: map[string]any{"label": label, "config": cfg}}
}

func TestRuntimeInputResolution(t *testing.T) {
	rt := &Runtime{Inputs: map[string]any{
		"user_prompt": "chocolate club",
		"n2":          map[string]any{"text": "by id"},
		"Ref":         map[string]any{"url": "https://example.com/a.png"},
	}}

	n1 := inputNode("n1", "textInput", "user_prompt", "Text Input", nil)
	if v, ok := rt.runtimeInput(&n1); !ok || v != "chocolate club" {
		t.Fatalf("by key: got %q %v", v, ok)
	}
	n2 := inputNode("n2", "textInput", "", "Text Input", nil)
	if v, ok := rt.runtimeInput(&n2); !ok || v != "by id" {
		t.Fatalf("by id with object value: got %q %v", v, ok)
	}
	n3 := inputNode("n3", "imageInput", "", "Ref", nil)
	if v, ok := rt.runtimeInput(&n3); !ok || v != "https://example.com/a.png" {
		t.Fatalf("by label with url object: got %q %v", v, ok)
	}
	n4 := inputNode("n4", "textInput", "missing", "Other", map[string]any{"value": "default"})
	if _, ok := rt.runtimeInput(&n4); ok {
		t.Fatalf("expected no runtime value")
	}
	out, err := rt.runTextInput(&RunInput{Node: &n4, Config: nodeConfig(&n4)})
	if err != nil || out.Value["text"] != "default" {
		t.Fatalf("default value: got %v %v", out, err)
	}
	n5 := inputNode("n5", "imageInput", "reference", "Image", nil)
	if _, err := rt.runImageInput(&RunInput{Node: &n5, Config: nodeConfig(&n5)}); err == nil {
		t.Fatalf("expected error for missing required image input")
	}
}

func TestRuntimeInputTypesSaysTheTruth(t *testing.T) {
	// La liste est un miroir : l'éditeur décide s'il propose un nom à un nœud,
	// et c'est le moteur qui décide si ce nom sert. Une liste écrite à la main
	// et jamais confrontée finit par promettre un nom qui ne remplit rien — ou
	// par taire celui qui aurait marché.
	//
	// Alors on ne relit pas la liste : on exécute chacun de ses nœuds avec une
	// valeur d'exécution et on regarde si elle a servi.
	rt := &Runtime{Inputs: map[string]any{
		"n":     "la valeur demandée",
		"image": "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7",
	}}

	for _, typ := range RuntimeInputTypes {
		key := "n"
		if typ == "imageInput" {
			key = "image"
		}
		node := inputNode(key, typ, "", "", nil)
		out, err := rt.executeWithInput(t.Context(), &RunInput{Node: &node, Config: nodeConfig(&node)})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		switch typ {
		case "textInput":
			if out.Value["text"] != "la valeur demandée" {
				t.Errorf("**%s est annoncé comme remplissable et ne l'est pas** : %v", typ, out.Value)
			}
		case "imageInput":
			if out.Type != "image" {
				t.Errorf("**%s est annoncé comme remplissable et ne l'est pas** : %v", typ, out)
			}
		}
	}

	// fileInput n'y est pas, et c'est un choix : son chemin se paramètre par un
	// {{input:…}} dans sa configuration, ce qui est un autre geste. L'y ajouter
	// ferait proposer un nom qui ne remplirait rien.
	for _, typ := range RuntimeInputTypes {
		if typ == "fileInput" {
			t.Error("fileInput ne lit pas les valeurs d'exécution par son nom")
		}
	}
}
