package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Zyvro/Zyvro-engine/providers"
)

// What a built-in node produces, recorded once and then held against every
// later implementation of it.
//
// The node types moved out of the Go switch and into Lua in the bundled pack,
// and "the Lua one behaves like the Go one" is not something anybody can check
// by reading two files side by side: the interesting part is the shape of the
// value that comes out, down to which keys are present and what the stored
// media was called. So the whole catalogue was run through the switch while it
// was still the switch, and every output was written to testdata/builtins as
// JSON. Those files are the record of the Go behaviour. This test replays the
// same graphs against whatever runs them today and fails on any difference.
//
// The goldens are regenerated with `go test ./engine -run Builtin -update`,
// which is how they were captured in the first place. Regenerating them is
// always a decision to change behaviour, and the diff is what the change is.
//
// No case reaches a real provider. The two fake servers below are a Gemini and
// an OpenAI endpoint on loopback, and every model the graphs mention is one of
// them.

var updateGoldens = flag.Bool("update", false, "rewrite the captured built-in outputs in testdata/builtins")

// ---------- the recorded shape ----------

// capture is everything one graph run leaves behind that anybody downstream can
// see: the outputs, the failure if it failed, what the agent did, and what
// reached the media store. All four matter — removeBackground is only correct
// if it also saved its two matte passes, and a node that stops producing
// Value["url"] has broken every workflow that previewed it.
type capture struct {
	Outputs map[string]*NodeOutput `json:"outputs,omitempty"`
	Error   string                 `json:"error,omitempty"`
	Trace   []AgentStep            `json:"trace,omitempty"`
	Media   []string               `json:"media,omitempty"`
}

// ---------- deterministic images ----------

// solidWithBorder builds an image with a uniform border and a different centre,
// which is the shape every image node here needs: the border is what
// detectBackgroundColors reads and what borderLuma measures, and the centre is
// the subject that has to survive.
func solidWithBorder(w, h int, border, centre color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := border
			if x >= w/4 && x < w-w/4 && y >= h/4 && y < h-h/4 {
				c = centre
			}
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

var (
	white = color.NRGBA{255, 255, 255, 255}
	black = color.NRGBA{0, 0, 0, 255}
	red   = color.NRGBA{220, 40, 40, 255}
	blue  = color.NRGBA{40, 80, 220, 255}
)

func pngDataURL(data []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)
}

// ---------- the fake providers ----------

// fakeGemini answers the generateContent endpoint for both the image models and
// the vision models. Which one it is answering is decided by the prompt, the
// same way the real endpoint decides what to return from what it was sent: a
// matte pass names the colour it wants, so the two passes can be told apart and
// answered with an image whose border is actually that colour.
type fakeGemini struct {
	*httptest.Server
	prompts []string
}

func newFakeGemini(t *testing.T) *fakeGemini {
	t.Helper()
	f := &fakeGemini{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		prompt := ""
		for _, c := range body.Contents {
			for _, p := range c.Parts {
				if p.Text != "" {
					prompt = p.Text
				}
			}
		}
		f.prompts = append(f.prompts, prompt)

		w.Header().Set("Content-Type", "application/json")
		// A vision model is asked for words; everything else is asked for an
		// image. The path carries the model name, which is what says which.
		if strings.Contains(r.URL.Path, "vision-model") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"candidates": []any{map[string]any{"content": map[string]any{
					"parts": []any{map[string]any{"text": "{\"subject\": \"a red square\", \"tags\": []}"}},
				}}},
			})
			return
		}

		var img []byte
		switch {
		case strings.Contains(prompt, "pure white"):
			img = solidWithBorder(16, 16, white, red)
		case strings.Contains(prompt, "pure black"):
			img = solidWithBorder(16, 16, black, red)
		default:
			img = solidWithBorder(8, 8, blue, red)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []any{map[string]any{"content": map[string]any{
				"parts": []any{map[string]any{"inlineData": map[string]string{
					"mimeType": "image/png",
					"data":     base64.StdEncoding.EncodeToString(img),
				}}},
			}}},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

// scriptedOpenAI answers chat completions from a fixed list, so a Brain's
// several turns are a script rather than a model.
type scriptedOpenAI struct {
	*httptest.Server
	replies []map[string]any
	calls   int
}

func newScriptedOpenAI(t *testing.T, replies ...map[string]any) *scriptedOpenAI {
	t.Helper()
	f := &scriptedOpenAI{replies: replies}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := f.replies[len(f.replies)-1]
		if f.calls < len(f.replies) {
			reply = f.replies[f.calls]
		}
		f.calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": reply}},
		})
	}))
	t.Cleanup(f.Close)
	return f
}

func assistantSays(text string) map[string]any {
	return map[string]any{"role": "assistant", "content": text}
}

func assistantCalls(id, name, args string) map[string]any {
	return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
		map[string]any{"id": id, "type": "function", "function": map[string]any{
			"name": name, "arguments": args,
		}},
	}}
}

// ---------- the fake host ----------

// recordingStore is a MediaStore that invents a stable URL and remembers every
// filename it was given. The filenames are behaviour: "pass-white.png" next to
// a transparent result is how somebody debugs a bad matte.
type recordingStore struct{ saved []string }

func (s *recordingStore) SaveMedia(execID, nodeID, filename string, data []byte, mime string) (string, error) {
	s.saved = append(s.saved, fmt.Sprintf("%s/%s/%s (%s, %d bytes)", execID, nodeID, filename, mime, len(data)))
	return "https://media.test/" + execID + "/" + nodeID + "/" + filename, nil
}

// ---------- the cases ----------

type builtinCase struct {
	name   string
	graph  *Graph
	inputs map[string]any
	// files, when set, gives the run a project folder.
	files *fakeFiles
	// external, when set, gives the run a host tool surface.
	external ExternalTools
	// replies scripts the text provider; empty means the default single reply.
	replies []map[string]any
}

func node(id, typ string, cfg map[string]any) GraphNode {
	if cfg == nil {
		cfg = map[string]any{}
	}
	return GraphNode{ID: id, Type: typ, Data: map[string]any{"config": cfg}}
}

func dataEdge(id, from, to string) GraphEdge {
	return GraphEdge{ID: id, Source: from, Target: to, SourceHandle: "out", TargetHandle: "in", Type: "data"}
}

// toolEdge binds a tool node to a Brain. It runs from the tool to the Brain,
// which is the direction ToolNodesOf reads.
func toolEdge(id, tool, brain string) GraphEdge {
	return GraphEdge{ID: id, Source: tool, Target: brain, Type: "tool"}
}

// imageSource is a textInput-free way to put an image on a node's input: an
// imageInput node carrying a data URL in its config.
func imageSource(id string, data []byte) GraphNode {
	return node(id, "imageInput", map[string]any{"dataUrl": pngDataURL(data)})
}

func builtinCases() []builtinCase {
	sixteen := solidWithBorder(16, 16, white, red)
	small := solidWithBorder(8, 8, white, blue)

	return []builtinCase{
		{
			name: "textInput_static",
			graph: &Graph{
				Nodes: []GraphNode{node("A", "textInput", map[string]any{"value": "hello {{input:who}}"}), node("B", "preview", nil)},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")},
			},
			inputs: map[string]any{"who": "world"},
		},
		{
			name: "textInput_from_runtime_input",
			graph: &Graph{
				Nodes: []GraphNode{node("A", "textInput", map[string]any{"value": "ignored", "inputKey": "topic"}), node("B", "output", nil)},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")},
			},
			inputs: map[string]any{"topic": "the real one"},
		},
		{
			name: "imageInput_from_config",
			graph: &Graph{
				Nodes: []GraphNode{imageSource("A", small), node("B", "preview", nil)},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")},
			},
		},
		{
			name:  "imageInput_missing",
			graph: &Graph{Nodes: []GraphNode{node("A", "imageInput", map[string]any{"inputKey": "picture"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "mergeText",
			graph: &Graph{
				Nodes: []GraphNode{
					node("A", "textInput", map[string]any{"value": "first"}),
					node("B", "textInput", map[string]any{"value": "second"}),
					node("C", "mergeText", map[string]any{"separator": " -- "}),
				},
				Edges: []GraphEdge{dataEdge("e1", "A", "C"), dataEdge("e2", "B", "C")},
			},
		},
		{
			name: "mergeText_defaults_and_json",
			graph: &Graph{
				Nodes: []GraphNode{
					node("A", "textInput", map[string]any{"value": "a line"}),
					imageSource("B", small),
					node("C", "mergeText", nil),
				},
				Edges: []GraphEdge{dataEdge("e1", "A", "C"), dataEdge("e2", "B", "C")},
			},
		},
		{
			name: "llm_text",
			graph: &Graph{
				Nodes: []GraphNode{
					node("A", "textInput", map[string]any{"value": "the upstream prompt"}),
					node("B", "llm", map[string]any{"system": "be terse", "maxTokens": 128, "temperature": 0.2, "provider": "openai"}),
				},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")},
			},
			replies: []map[string]any{assistantSays("a short answer")},
		},
		{
			name: "llm_json",
			graph: &Graph{Nodes: []GraphNode{
				node("A", "llm", map[string]any{"prompt": "give me json", "jsonOutput": true, "provider": "openai"}),
				node("B", "preview", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			replies: []map[string]any{assistantSays("here you go: {\"ok\": true, \"items\": [1, 2]} and that is all")},
		},
		{
			name:  "llm_without_a_prompt",
			graph: &Graph{Nodes: []GraphNode{node("A", "llm", map[string]any{"provider": "openai"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "generateImage_from_config",
			graph: &Graph{Nodes: []GraphNode{
				node("A", "generateImage", map[string]any{"prompt": "a cat {{input:style}}", "aspectRatio": "16:9", "imageSize": "2K"}),
				node("B", "preview", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			inputs: map[string]any{"style": "in watercolour"},
		},
		{
			name: "generateImage_prompt_from_upstream_with_reference",
			graph: &Graph{Nodes: []GraphNode{
				node("A", "textInput", map[string]any{"value": "a dog"}),
				imageSource("R", small),
				node("B", "generateImage", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "R", "B")}},
		},
		{
			name:  "generateImage_without_a_prompt",
			graph: &Graph{Nodes: []GraphNode{node("A", "generateImage", nil), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "editImage",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", small),
				node("B", "editImage", map[string]any{"prompt": "make it warmer"}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "editImage_prompt_from_upstream",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", small),
				node("P", "textInput", map[string]any{"value": "add a hat"}),
				node("B", "editImage", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "P", "B")}},
		},
		{
			name:  "editImage_without_an_image",
			graph: &Graph{Nodes: []GraphNode{node("A", "textInput", map[string]any{"value": "x"}), node("B", "editImage", map[string]any{"prompt": "p"})}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name:  "editImage_without_a_prompt",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "editImage", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "removeBackground_programmatic",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", sixteen),
				node("B", "removeBackground", map[string]any{"mode": "programmatic", "tolerance": 30}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "removeBackground_ai_two_pass",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", sixteen),
				node("B", "removeBackground", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name:  "removeBackground_without_an_image",
			graph: &Graph{Nodes: []GraphNode{node("A", "textInput", map[string]any{"value": "x"}), node("B", "removeBackground", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "rotateImage_default",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "rotateImage", nil)},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "rotateImage_270",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "rotateImage", map[string]any{"degrees": 270})},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name:  "rotateImage_without_an_image",
			graph: &Graph{Nodes: []GraphNode{node("A", "textInput", map[string]any{"value": "x"}), node("B", "rotateImage", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "flipImage_default",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "flipImage", nil)},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "flipImage_vertical",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "flipImage", map[string]any{"axis": "v"})},
				Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "vision_text",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", small),
				node("B", "vision", map[string]any{"instruction": "describe {{input:what}}", "model": "vision-model"}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			inputs: map[string]any{"what": "this picture"},
		},
		{
			name: "vision_json",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", small),
				imageSource("A2", sixteen),
				node("B", "vision", map[string]any{"instruction": "tag it", "model": "vision-model", "jsonOutput": true}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "A2", "B")}},
		},
		{
			name: "vision_instruction_from_upstream",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", small),
				node("T", "textInput", map[string]any{"value": "what colour is it"}),
				node("B", "vision", map[string]any{"model": "vision-model"}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "T", "B")}},
		},
		{
			name:  "vision_without_an_image",
			graph: &Graph{Nodes: []GraphNode{node("T", "textInput", map[string]any{"value": "describe"}), node("B", "vision", map[string]any{"model": "vision-model"})}, Edges: []GraphEdge{dataEdge("e1", "T", "B")}},
		},
		{
			name:  "vision_without_an_instruction",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "vision", map[string]any{"model": "vision-model"})}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "voxelPreview",
			graph: &Graph{Nodes: []GraphNode{
				imageSource("A", small),
				imageSource("A2", sixteen),
				node("B", "voxelPreview", map[string]any{"shape": "sphere"}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "A2", "B")}},
		},
		{
			name:  "voxelPreview_without_images",
			graph: &Graph{Nodes: []GraphNode{node("T", "textInput", map[string]any{"value": "x"}), node("B", "voxelPreview", nil)}, Edges: []GraphEdge{dataEdge("e1", "T", "B")}},
		},
		{
			name:     "zyvroTools_with_a_host_surface",
			graph:    &Graph{Nodes: []GraphNode{node("A", "zyvroTools", nil), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			external: &stubTools{},
		},
		{
			name:  "zyvroTools_without_a_host_surface",
			graph: &Graph{Nodes: []GraphNode{node("A", "zyvroTools", nil), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name: "brain_calls_a_tool_then_answers",
			graph: &Graph{Nodes: []GraphNode{
				node("A", "textInput", map[string]any{"value": "draw something"}),
				node("B", "brain", map[string]any{"provider": "openai", "maxSteps": 4}),
				node("T", "generateImage", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), toolEdge("t1", "T", "B")}},
			replies: []map[string]any{
				assistantCalls("call_1", "generateImage_T", `{"instructions":"a blue square"}`),
				assistantSays("Here is the square."),
			},
		},
		{
			name: "brain_runs_out_of_steps",
			graph: &Graph{Nodes: []GraphNode{
				node("B", "brain", map[string]any{"goal": "keep going", "provider": "openai", "maxSteps": 1}),
				node("T", "llm", map[string]any{"provider": "openai"}),
				node("P", "preview", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "B", "P"), toolEdge("t1", "T", "B")}},
			replies: []map[string]any{assistantCalls("call_1", "llm_T", `{"instructions":"think"}`)},
		},
		{
			name: "brain_without_a_goal",
			graph: &Graph{Nodes: []GraphNode{
				node("B", "brain", map[string]any{"provider": "openai"}),
				node("T", "llm", nil),
				node("P", "preview", nil),
			}, Edges: []GraphEdge{dataEdge("e1", "B", "P"), toolEdge("t1", "T", "B")}},
		},
		{
			name:  "brain_without_tools",
			graph: &Graph{Nodes: []GraphNode{node("B", "brain", map[string]any{"goal": "do it", "provider": "openai"}), node("P", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "B", "P")}},
		},
		{
			name:  "preview_passes_its_input_through",
			graph: &Graph{Nodes: []GraphNode{imageSource("A", small), node("B", "preview", nil), node("C", "output", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "B", "C")}},
		},
		{
			name:  "preview_without_an_input",
			graph: &Graph{Nodes: []GraphNode{node("A", "textInput", map[string]any{"value": "x"}), node("B", "preview", nil), node("C", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B"), dataEdge("e2", "C", "B")}},
		},
		{
			// Il a refusé de tourner tant qu'aucun dos ne savait faire une
			// vidéo. Maintenant qu'il y en a deux, ce qu'il dit sans
			// description est ce que dit le nœud image : ce qui lui manque.
			name:  "generateVideo_says_what_it_needs",
			graph: &Graph{Nodes: []GraphNode{node("A", "generateVideo", nil), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
		{
			name:  "an_unknown_type_says_a_pack_may_be_missing",
			graph: &Graph{Nodes: []GraphNode{node("A", "notARealNode", nil), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
	}
}

// fileCases need a project folder, so they are built separately: the fake file
// access has to be created per run.
func fileCases() []builtinCase {
	textFiles := func() *fakeFiles {
		f := newFakeFiles()
		f.read["notes/today.txt"] = []byte("what happened today")
		f.mimes["notes/today.txt"] = "text/plain"
		f.read["data/config.json"] = []byte(`{"enabled": true, "retries": 3}`)
		f.read["art/logo.png"] = solidWithBorder(8, 8, white, blue)
		return f
	}
	return []builtinCase{
		{
			name:  "fileInput_text",
			graph: &Graph{Nodes: []GraphNode{node("A", "fileInput", map[string]any{"path": "notes/{{input:name}}"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			files: textFiles(), inputs: map[string]any{"name": "today.txt"},
		},
		{
			name:  "fileInput_json",
			graph: &Graph{Nodes: []GraphNode{node("A", "fileInput", map[string]any{"path": "data/config.json"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			files: textFiles(),
		},
		{
			name:  "fileInput_image",
			graph: &Graph{Nodes: []GraphNode{node("A", "fileInput", map[string]any{"path": "art/logo.png"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			files: textFiles(),
		},
		{
			name:  "fileInput_missing",
			graph: &Graph{Nodes: []GraphNode{node("A", "fileInput", map[string]any{"path": "nope.txt"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			files: textFiles(),
		},
		{
			name: "fileOutput_passes_the_value_through",
			graph: &Graph{Nodes: []GraphNode{
				node("A", "textInput", map[string]any{"value": "the contents"}),
				node("B", "fileOutput", map[string]any{"path": "out/result.txt", "createDirs": true}),
			}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
			files: textFiles(),
		},
		{
			name:  "fileInput_without_a_project_folder",
			graph: &Graph{Nodes: []GraphNode{node("A", "fileInput", map[string]any{"path": "notes/today.txt"}), node("B", "preview", nil)}, Edges: []GraphEdge{dataEdge("e1", "A", "B")}},
		},
	}
}

// stubTools is a fixed host tool surface. What zyvroTools produces is the list
// of names, so the schemas only have to have names.
type stubTools struct{}

func (s *stubTools) Schemas() []providers.ToolSchema {
	return []providers.ToolSchema{
		{Name: "zyvro_list_workflows", Description: "List the workflows"},
		{Name: "zyvro_run_workflow", Description: "Run one of them"},
	}
}

func (s *stubTools) Call(context.Context, string, map[string]any) (string, []string, string, error) {
	return "", nil, "", fmt.Errorf("stub tools are never called")
}

// ---------- the run ----------

func TestBuiltinBehaviourIsUnchanged(t *testing.T) {
	cases := append(builtinCases(), fileCases()...)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runBuiltinCase(t, tc)
			golden := filepath.Join("testdata", "builtins", tc.name+".json")
			encoded, err := json.MarshalIndent(got, "", "  ")
			if err != nil {
				t.Fatalf("encode capture: %v", err)
			}
			encoded = append(encoded, '\n')

			if *updateGoldens {
				if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(golden, encoded, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}

			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("no captured behaviour for %s: %v\nrun `go test ./engine -run Builtin -update` only when you mean to change what a node produces", tc.name, err)
			}
			if !bytes.Equal(bytes.TrimSpace(want), bytes.TrimSpace(encoded)) {
				t.Errorf("%s no longer produces what it produced before.\n--- captured ---\n%s\n--- now ---\n%s", tc.name, want, encoded)
			}
		})
	}
}

func runBuiltinCase(t *testing.T, tc builtinCase) capture {
	t.Helper()
	gemini := newFakeGemini(t)
	replies := tc.replies
	if len(replies) == 0 {
		replies = []map[string]any{assistantSays("a model answer")}
	}
	text := newScriptedOpenAI(t, replies...)

	cfg := &providers.Config{
		TextProvider:  "openai",
		OpenAIBaseURL: text.URL,
		OpenAIAPIKey:  "test-key-not-a-real-one",
		OpenAIModel:   "fake-text-model",
		GeminiBaseURL: gemini.URL,
		GoogleAPIKey:  "test-key-not-a-real-one",
		ImageModel:    "image-model",
		VisionModel:   "vision-model",
	}
	store := &recordingStore{}
	rt := NewRuntime("exec-fixed", tc.graph, cfg, store, tc.inputs)
	if tc.files != nil {
		rt.Files = tc.files
	}
	if tc.external != nil {
		rt.External = tc.external
	}

	out := capture{Media: store.saved}
	if err := rt.Execute(context.Background()); err != nil {
		out.Error = err.Error()
	}
	if len(rt.Outputs) > 0 {
		out.Outputs = map[string]*NodeOutput{}
		for id, o := range rt.Outputs {
			out.Outputs[id] = o
		}
	}
	out.Trace = rt.AgentTrace
	// In order, not sorted: the two matte passes are saved white first and then
	// black, and a run that started saving them the other way round would be a
	// run that did the passes the other way round.
	out.Media = store.saved
	return out
}
