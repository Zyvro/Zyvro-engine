package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg" // decode support
	"image/png"
	"math"
	"sort"
	"strings"
)

// decodeImage decodes PNG/JPEG/GIF bytes. Returns the decoded image and its
// bounds.
func decodeImage(data []byte) (image.Image, string, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("cannot decode image: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("cannot decode image: %w", err)
	}
	_ = cfg
	return img, format, nil
}

// encodePNG encodes an image with alpha as PNG.
func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mattingExtract implements the two-pass matte protocol:
// white-pass and black-pass images of identical dimensions are compared
// per-pixel; alpha = 1 - dist(pixelWhite, pixelBlack) / dist(white, black),
// and the foreground color is recovered from the black pass: c = cB / alpha.
// This mirrors the reference implementation in Golf-Splash-thumbnail-Manager
// (server.mjs extractAlphaTwoPass).
func mattingExtract(whiteImg, blackImg image.Image) (*image.NRGBA, error) {
	bounds := whiteImg.Bounds()
	if !bounds.Eq(blackImg.Bounds()) {
		return nil, fmt.Errorf("dimension mismatch: white (%dx%d) and black (%dx%d) passes differ",
			bounds.Dx(), bounds.Dy(), blackImg.Bounds().Dx(), blackImg.Bounds().Dy())
	}

	const bgDist = 441.6729559300637 // sqrt(3 * 255^2)
	const matteLow, matteHigh = 0.04, 0.92
	out := image.NewNRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))

	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			wr, wg, wb, _ := whiteImg.At(x, y).RGBA()
			br, bg, bb, _ := blackImg.At(x, y).RGBA()

			// Work on 8-bit values.
			wr8, wg8, wb8 := float64(wr>>8), float64(wg>>8), float64(wb>>8)
			br8, bg8, bb8 := float64(br>>8), float64(bg>>8), float64(bb>>8)

			dist := math.Sqrt((wr8-br8)*(wr8-br8) + (wg8-bg8)*(wg8-bg8) + (wb8-bb8)*(wb8-bb8))
			alpha := 1 - dist/bgDist
			// Matte cleanup: the passes are JPEG re-encodes of a regenerated
			// image, so the background never reaches exactly 0 and the
			// subject never exactly 1. Stretch the useful range so noise
			// becomes fully transparent and the subject fully opaque; edges
			// keep their soft antialiasing in between.
			alpha = (alpha - matteLow) / (matteHigh - matteLow)
			if alpha < 0 {
				alpha = 0
			}
			if alpha > 1 {
				alpha = 1
			}

			var rOut, gOut, bOut float64
			if alpha > 0.01 {
				rOut = br8 / alpha
				gOut = bg8 / alpha
				bOut = bb8 / alpha
			}

			out.Set(x, y, color.NRGBA{
				R: clamp255(rOut),
				G: clamp255(gOut),
				B: clamp255(bOut),
				A: uint8(math.Round(alpha * 255)),
			})
		}
	}
	return out, nil
}

func clamp255(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(math.Round(v))
}

// imageDataURL produces a data: URL for embedding in node output JSON.
func imageDataURL(mime string, data []byte) string {
	return fmt.Sprintf("data:%s;base64,%s", mime, base64.StdEncoding.EncodeToString(data))
}

// parseDataURL extracts mime + bytes from a data: URL. Empty bytes with
// error nil means "no data URL present".
func parseDataURL(s string) (string, []byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil, nil
	}
	const prefix = "data:"
	if !strings.HasPrefix(s, prefix) {
		return "", nil, fmt.Errorf("not a data URL")
	}
	rest := s[len(prefix):]
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return "", nil, fmt.Errorf("malformed data URL")
	}
	meta := rest[:comma]
	mime := meta
	if i := strings.Index(meta, ";"); i >= 0 {
		mime = meta[:i]
	}
	data, err := base64.StdEncoding.DecodeString(rest[comma+1:])
	if err != nil {
		return "", nil, fmt.Errorf("invalid base64 in data URL: %w", err)
	}
	return mime, data, nil
}

// jsonCompact marshals v or returns "{}" on failure.
func jsonCompact(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// borderLuma returns the mean 8-bit luminance of a thin frame around the
// image. The matte passes are validated with it: a white pass must be
// bright at the edges, a black pass dark, otherwise the model ignored the
// background instruction (it happens) and the alpha would be meaningless.
func borderLuma(img image.Image) float64 {
	b := img.Bounds()
	if b.Dx() < 8 || b.Dy() < 8 {
		return 128
	}
	var sum float64
	var n int
	sample := func(x, y int) {
		r, g, bl, _ := img.At(x, y).RGBA()
		sum += float64((r>>8)+(g>>8)+(bl>>8)) / 3
		n++
	}
	for x := b.Min.X; x < b.Max.X; x += 4 {
		sample(x, b.Min.Y+2)
		sample(x, b.Max.Y-3)
	}
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		sample(b.Min.X+2, y)
		sample(b.Max.X-3, y)
	}
	return sum / float64(n)
}

// rgbColor is an 8-bit RGB triple, the working unit of the programmatic
// background detector.
type rgbColor struct{ R, G, B float64 }

func (c rgbColor) distTo(o rgbColor) float64 {
	dr, dg, db := c.R-o.R, c.G-o.G, c.B-o.B
	return math.Sqrt(dr*dr + dg*dg + db*db)
}

// detectBackgroundColors samples a thin border frame of the image, buckets
// the sampled colors on a 5-bit-per-channel histogram and returns the
// dominant buckets until they cover 90% of the border samples. Sampling the
// border assumes the subject does not touch the frame, which is how these
// images are almost always composed; the AI two-pass mode exists for the
// other cases.
func detectBackgroundColors(img image.Image) []rgbColor {
	b := img.Bounds()
	if b.Dx() < 8 || b.Dy() < 8 {
		return nil
	}
	type bucket struct {
		count int
		sum   rgbColor
	}
	hist := map[[3]uint8]*bucket{}
	sample := func(x, y int) {
		r, g, bl, _ := img.At(x, y).RGBA()
		// 5 bits per channel of the 8-bit value (>>8 then >>3).
		key := [3]uint8{uint8(r>>8) >> 3, uint8(g>>8) >> 3, uint8(bl>>8) >> 3}
		bk, ok := hist[key]
		if !ok {
			bk = &bucket{}
			hist[key] = bk
		}
		bk.count++
		bk.sum.R += float64(r >> 8)
		bk.sum.G += float64(g >> 8)
		bk.sum.B += float64(bl >> 8)
	}
	for x := b.Min.X; x < b.Max.X; x += 3 {
		sample(x, b.Min.Y+2)
		sample(x, b.Max.Y-3)
	}
	for y := b.Min.Y; y < b.Max.Y; y += 3 {
		sample(b.Min.X+2, y)
		sample(b.Max.X-3, y)
	}

	total := 0
	type entry struct {
		color rgbColor
		count int
	}
	entries := make([]entry, 0, len(hist))
	for _, bk := range hist {
		if bk.count < 2 {
			continue // single-pixel noise
		}
		entries = append(entries, entry{
			color: rgbColor{R: bk.sum.R / float64(bk.count), G: bk.sum.G / float64(bk.count), B: bk.sum.B / float64(bk.count)},
			count: bk.count,
		})
		total += bk.count
	}
	if total == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].count > entries[j].count })

	var bgs []rgbColor
	var covered int
	for _, e := range entries {
		bgs = append(bgs, e.color)
		covered += e.count
		if float64(covered)/float64(total) >= 0.9 {
			break
		}
	}
	// A background made of many equally-weighted colors is a scene, not a
	// backdrop: keep only the dominant colors so we never erase a subject
	// that happens to sit near the frame.
	if len(bgs) > 8 {
		bgs = bgs[:8]
	}
	return bgs
}

// rotateImage returns img rotated by the given degrees. Only multiples of
// 90 are supported; other values fall back to the nearest right angle.
func rotateImage(img image.Image, degrees int) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	d := ((degrees % 360) + 360) % 360
	// Snap to the nearest multiple of 90.
	d = ((d + 45) / 90) * 90 % 360
	switch d {
	case 90:
		out := image.NewNRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.SetNRGBA(h-1-y, x, color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
			}
		}
		return out
	case 180:
		out := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.SetNRGBA(w-1-x, h-1-y, color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
			}
		}
		return out
	case 270:
		out := image.NewNRGBA(image.Rect(0, 0, h, w))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.SetNRGBA(y, w-1-x, color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
			}
		}
		return out
	default:
		out := image.NewNRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
				out.SetNRGBA(x, y, color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)})
			}
		}
		return out
	}
}

// flipImage mirrors img horizontally ("h") or vertically ("v"). Unknown
// axes return the original image unchanged.
func flipImage(img image.Image, axis string) *image.NRGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			px := color.NRGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)}
			switch axis {
			case "h":
				out.SetNRGBA(w-1-x, y, px)
			case "v":
				out.SetNRGBA(x, h-1-y, px)
			default:
				out.SetNRGBA(x, y, px)
			}
		}
	}
	return out
}

// removeBackgroundRange erases every pixel within tolerance of one of the
// detected background colors. Alpha ramps smoothly from 0 (background) to
// 255 (foreground) across the tolerance band so edges stay anti-aliased;
// the ramp runs on RGB distance, per background color.
func removeBackgroundRange(img image.Image, bgs []rgbColor, tolerance float64) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	if tolerance <= 0 {
		tolerance = 30
	}
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			px := rgbColor{R: float64(r >> 8), G: float64(g >> 8), B: float64(bl >> 8)}

			minDist := math.MaxFloat64
			for _, bg := range bgs {
				if d := px.distTo(bg); d < minDist {
					minDist = d
				}
			}

			var alpha float64
			switch {
			case minDist <= tolerance*0.5:
				alpha = 0
			case minDist >= tolerance:
				alpha = 1
			default:
				alpha = (minDist - tolerance*0.5) / (tolerance * 0.5)
			}

			out.Set(x, y, color.NRGBA{
				R: clamp255(px.R),
				G: clamp255(px.G),
				B: clamp255(px.B),
				A: uint8(math.Round(alpha * 255)),
			})
		}
	}
	return out
}
