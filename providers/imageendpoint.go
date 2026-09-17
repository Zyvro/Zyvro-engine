package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"strings"
)

// An image server running on the machine the person is sitting at, spoken to
// the way every other image server is spoken to.
//
// The first version of this file addressed one particular server's own routes,
// which worked and was wrong: it would have been Zyvro code that knows about
// one person's file, and nobody else's server would have fitted it. The images
// half of the OpenAI API is the protocol these things actually agree on —
// POST /v1/images/generations to draw, POST /v1/images/edits to redraw with
// references — so that is what this asks for, and any server implementing it
// works here without our knowing it exists.
//
// It is the same protocol as endpoints.go, the other half of it: chat there,
// images here. Different shapes, one dialect, one setting.

// endpointImageGenerate renders a prompt, or edits the references against it.
func (c *Config) endpointImageGenerate(ctx context.Context, provider, model, prompt, aspectRatio, imageSize string, references []ImageResult) (*ImageResult, error) {
	e := c.endpointFor(provider)
	if e.URL == "" {
		return nil, fmt.Errorf("%s has no address configured", provider)
	}
	if strings.TrimSpace(prompt) == "" {
		// The API refuses this with a validation error naming a field. This
		// says the same thing in the graph's vocabulary.
		return nil, fmt.Errorf("the image node has no prompt")
	}
	if strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(e.Model)
	}
	size := imageEndpointSize(aspectRatio, imageSize)

	var raw []byte
	var err error
	if len(references) > 0 {
		raw, err = c.imageEdit(ctx, e, model, prompt, size, references)
	} else {
		raw, err = c.imageGeneration(ctx, e, model, prompt, size)
	}
	if err != nil {
		return nil, endpointImageError(err)
	}

	var parsed struct {
		Data []struct {
			B64  string `json:"b64_json"`
			URL  string `json:"url"`
			Path string `json:"path"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return nil, fmt.Errorf("that address did not answer with an image")
	}
	if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
		return nil, &ProviderError{Code: "provider_error", Message: parsed.Error.Message}
	}
	if len(parsed.Data) == 0 {
		return nil, fmt.Errorf("the image server returned no image")
	}
	if parsed.Data[0].B64 == "" {
		if parsed.Data[0].URL != "" || parsed.Data[0].Path != "" {
			// Asked for b64_json and given a link instead. Not followed: a link
			// from a server we were pointed at is a fetch we did not decide to
			// make, and on a local server it is usually a file path this
			// process has no business opening.
			return nil, fmt.Errorf("that server answered with a link rather than the image — it needs to support response_format=b64_json")
		}
		return nil, fmt.Errorf("the image server returned no image")
	}
	data, err := base64.StdEncoding.DecodeString(parsed.Data[0].B64)
	if err != nil {
		return nil, fmt.Errorf("the image server returned something that is not an image")
	}
	return &ImageResult{Data: data, MimeType: "image/png"}, nil
}

// imageGeneration is the plain draw-me-this call.
func (c *Config) imageGeneration(ctx context.Context, e Endpoint, model, prompt, size string) ([]byte, error) {
	body, err := json.Marshal(map[string]any{
		"model":  model,
		"prompt": prompt,
		"n":      1,
		"size":   size,
		// Asked for explicitly: the default in this API is a link, and a link
		// means a second fetch, an expiry, and a file we would have to reach.
		"response_format": "b64_json",
	})
	if err != nil {
		return nil, err
	}
	return postJSON(ctx, e.URL+"/images/generations", e.Key, body)
}

// imageEdit is the same call with references, which the protocol sends as
// multipart because they are files.
func (c *Config) imageEdit(ctx context.Context, e Endpoint, model, prompt, size string, references []ImageResult) ([]byte, error) {
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	for i, ref := range references {
		// image[] is what the current API takes for more than one; a server
		// that only accepts a single "image" reads the first part under either
		// name, so this stays the documented spelling.
		part, err := form.CreateFormFile("image[]", fmt.Sprintf("reference-%d.png", i+1))
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(ref.Data); err != nil {
			return nil, err
		}
	}
	for field, value := range map[string]string{
		"prompt":          prompt,
		"model":           model,
		"size":            size,
		"n":               "1",
		"response_format": "b64_json",
	} {
		if value == "" {
			continue
		}
		if err := form.WriteField(field, value); err != nil {
			return nil, err
		}
	}
	if err := form.Close(); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", e.URL+"/images/edits", &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	if strings.TrimSpace(e.Key) != "" {
		req.Header.Set("Authorization", "Bearer "+e.Key)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 200 {
		return nil, normalizeHTTPError(resp.StatusCode, raw)
	}
	return raw, nil
}

// endpointImageError digs the server's own sentence out of the two shapes these
// servers answer with: OpenAI's {"error":{"message"}} and FastAPI's
// {"detail"} — the second because half the local implementations are FastAPI,
// and "the model is still loading" is the message that will actually happen.
func endpointImageError(err error) error {
	var pe *ProviderError
	if !errors.As(err, &pe) {
		return err
	}
	var parsed struct {
		Detail any `json:"detail"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(pe.Message), &parsed) == nil {
		if parsed.Error != nil && strings.TrimSpace(parsed.Error.Message) != "" {
			pe.Message = parsed.Error.Message
		} else if detail, ok := parsed.Detail.(string); ok && strings.TrimSpace(detail) != "" {
			pe.Message = detail
		}
	}
	return pe
}

// imageEndpointSize turns an aspect ratio into the protocol's "WIDTHxHEIGHT".
//
// About a megapixel, in multiples of 16. Both numbers come from the models
// rather than from us: the diffusion models these servers run are trained
// around that area and their pipelines require that stride, so a size chosen
// freely is either refused or quietly worse.
func imageEndpointSize(aspectRatio, imageSize string) string {
	w, h := imageEndpointDimensions(aspectRatio, imageSize)
	return fmt.Sprintf("%dx%d", w, h)
}

func imageEndpointDimensions(aspectRatio, imageSize string) (int, int) {
	pixels := 1_048_576.0
	switch strings.ToUpper(strings.TrimSpace(imageSize)) {
	case "2K":
		pixels = 2_359_296.0
	case "512":
		pixels = 262_144.0
	}

	ratio := 1.0
	switch strings.TrimSpace(aspectRatio) {
	case "16:9":
		ratio = 16.0 / 9.0
	case "9:16":
		ratio = 9.0 / 16.0
	case "4:3":
		ratio = 4.0 / 3.0
	case "3:4":
		ratio = 3.0 / 4.0
	case "3:2":
		ratio = 3.0 / 2.0
	case "2:3":
		ratio = 2.0 / 3.0
	}
	return round16(math.Sqrt(pixels * ratio)), round16(math.Sqrt(pixels / ratio))
}

// round16 keeps a side on the stride these pipelines need, inside the range
// they accept.
func round16(v float64) int {
	n := (int(v+8) / 16) * 16
	if n < 256 {
		return 256
	}
	if n > 2048 {
		return 2048
	}
	return n
}
