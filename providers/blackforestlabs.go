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

// Black Forest Labs, the second image backend.
//
// It exists so that somebody who has just signed up can produce an image before
// being asked for a credential. The platform holds one key here and hands out a
// small number of generations per account; everything else still runs on the
// user's own provider.
//
// The API is asynchronous, which is the whole shape of this file: a submit
// returns an id and a polling url, and the image only exists once a poll says
// "Ready". The finished image is a URL that expires in ten minutes, so it is
// downloaded here rather than passed on — a link that dies while a workflow is
// still running is not a result.

const bflDefaultBaseURL = "https://api.bfl.ai"

// bflModels are the model slugs this engine will call, and the list is an
// allow-list rather than a default because the slug becomes a path segment and
// the value comes out of a graph. A node that could name the path could name
// any endpoint on the host.
var bflModels = map[string]bool{
	"flux-2-klein-9b": true,
	"flux-2-klein-4b": true,
}

// BFLDefaultModel is what a node gets when it names no model. klein-9b is the
// cheap one: sub-second, open weights, and the reason a free allowance is
// affordable at all.
const BFLDefaultModel = "flux-2-klein-9b"

// bflPollInterval follows the vendor's own examples. Their guidance is 500ms;
// slower would make a sub-second model feel slow for no saving.
const bflPollInterval = 500 * time.Millisecond

type bflSubmitResponse struct {
	ID         string `json:"id"`
	PollingURL string `json:"polling_url"`
	Cost       any    `json:"cost"`
}

type bflResultResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result struct {
		Sample string `json:"sample"`
	} `json:"result"`
}

// bflImageGenerate submits a generation, waits for it, and returns the bytes.
func (c *Config) bflImageGenerate(ctx context.Context, model, prompt, aspectRatio, imageSize string, references []ImageResult) (*ImageResult, error) {
	if strings.TrimSpace(c.BFLAPIKey) == "" {
		return nil, fmt.Errorf("no Black Forest Labs key is configured")
	}
	slug := strings.ToLower(strings.TrimSpace(model))
	if slug == "" {
		slug = BFLDefaultModel
	}
	if !bflModels[slug] {
		return nil, fmt.Errorf("%q is not a Black Forest Labs model this engine calls", model)
	}

	width, height := bflDimensions(aspectRatio, imageSize)
	body := map[string]any{
		"prompt":        prompt,
		"width":         width,
		"height":        height,
		"output_format": "png",
	}
	// Reference images go in as raw base64, not as data URLs. The API takes up
	// to four; anything past that is dropped rather than refused, because a
	// graph with five references is still a graph somebody wants to run.
	for i, ref := range references {
		if i >= 4 {
			break
		}
		field := "input_image"
		if i > 0 {
			field = fmt.Sprintf("input_image_%d", i+1)
		}
		body[field] = base64.StdEncoding.EncodeToString(ref.Data)
	}

	polling, err := c.bflSubmit(ctx, slug, body)
	if err != nil {
		return nil, err
	}
	sample, err := c.bflAwait(ctx, polling)
	if err != nil {
		return nil, err
	}
	return c.bflDownload(ctx, sample)
}

func (c *Config) bflBaseURL() string {
	if u := strings.TrimSpace(c.BFLBaseURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return bflDefaultBaseURL
}

func (c *Config) bflSubmit(ctx context.Context, slug string, body map[string]any) (string, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bflBaseURL()+"/v1/"+slug, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-key", c.BFLAPIKey)

	res, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("image generation could not reach Black Forest Labs: %w", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("Black Forest Labs refused the request (%d): %s", res.StatusCode, bflSnippet(raw))
	}
	var out bflSubmitResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("Black Forest Labs returned something this engine cannot read: %w", err)
	}
	if out.PollingURL == "" {
		return "", fmt.Errorf("Black Forest Labs accepted the request but named no polling url")
	}
	return out.PollingURL, nil
}

// bflAwait polls until the task resolves, and returns the sample URL.
//
// The caller's context carries the deadline: a run already has one, and
// inventing a second here would mean a workflow that gives up while its own
// budget still says it may continue.
func (c *Config) bflAwait(ctx context.Context, pollingURL string) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("image generation gave up waiting for Black Forest Labs: %w", ctx.Err())
		case <-time.After(bflPollInterval):
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pollingURL, nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("x-key", c.BFLAPIKey)
		res, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("image generation lost contact with Black Forest Labs: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return "", fmt.Errorf("Black Forest Labs failed while generating (%d): %s", res.StatusCode, bflSnippet(raw))
		}
		var out bflResultResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return "", fmt.Errorf("Black Forest Labs returned something this engine cannot read: %w", err)
		}

		switch out.Status {
		case "Ready":
			if out.Result.Sample == "" {
				return "", fmt.Errorf("Black Forest Labs reported the image ready but sent no image")
			}
			return out.Result.Sample, nil
		case "Pending", "Reasoning", "Generating":
			// Still working. The vendor names three states for this and they
			// are all the same answer here.
		case "Request Moderated", "Content Moderated":
			// Not a failure of ours to retry: the prompt or the result was
			// refused, and saying which is the only useful thing to report.
			return "", fmt.Errorf("Black Forest Labs refused this prompt or its result (%s)", out.Status)
		case "Task not found":
			return "", fmt.Errorf("Black Forest Labs lost track of this generation")
		default:
			return "", fmt.Errorf("Black Forest Labs reported %q", out.Status)
		}
	}
}

// bflDownload fetches the finished image. The URL is good for ten minutes, so
// the bytes are taken now and stored by the caller.
func (c *Config) bflDownload(ctx context.Context, sampleURL string) (*ImageResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sampleURL, nil)
	if err != nil {
		return nil, err
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the generated image could not be downloaded: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("the generated image could not be downloaded (%d)", res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("the generated image could not be read: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("the generated image came back empty")
	}
	mime := res.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	if !strings.HasPrefix(mime, "image/") {
		// The URL is a delivery endpoint, so anything else means an error page
		// dressed as a download.
		mime = "image/png"
	}
	return &ImageResult{Data: data, MimeType: mime}, nil
}

// bflDimensions turns the aspect ratio and size a node speaks into the width
// and height this API wants.
//
// Rounded to multiples of 32 because that is what diffusion models accept, and
// a request the API silently rounds is a request whose output does not match
// what the node said it asked for.
func bflDimensions(aspectRatio, imageSize string) (int, int) {
	long := 1024
	switch strings.ToUpper(strings.TrimSpace(imageSize)) {
	case "2K":
		long = 1536
	case "512":
		long = 512
	}
	w, h := 1, 1
	switch strings.TrimSpace(aspectRatio) {
	case "16:9":
		w, h = 16, 9
	case "9:16":
		w, h = 9, 16
	case "4:3":
		w, h = 4, 3
	case "3:4":
		w, h = 3, 4
	case "3:2":
		w, h = 3, 2
	case "2:3":
		w, h = 2, 3
	}
	if w >= h {
		return round32(long), round32(long * h / w)
	}
	return round32(long * w / h), round32(long)
}

func round32(v int) int {
	r := (v / 32) * 32
	if r < 64 {
		return 64
	}
	return r
}

// bflSnippet keeps an error message readable when the body is a wall of JSON.
func bflSnippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	if s == "" {
		return "(empty response)"
	}
	return s
}
