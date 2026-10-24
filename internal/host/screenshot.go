package host

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"

	"hyperhand/internal/proto"
)

type screenshotRegion struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type screenshotOptions struct {
	Region *screenshotRegion
	// MaxSize is the output size rule: 0 fits the image to the model token budget (fitTokenBudget), -1 keeps the
	// original size, a positive value caps the longest side.
	MaxSize int
}

// Token budget for the default image size, after Claude's image sizing: about one token per 28x28 pixel tile, at
// most budgetMaxTokens tiles and budgetMaxSide pixels on the longest side.
const (
	budgetPxPerToken = 28
	budgetMaxSide    = 1568
	budgetMaxTokens  = 1568
)

func withinTokenBudget(w, h int) bool {
	tiles := func(n int) int { return (n + budgetPxPerToken - 1) / budgetPxPerToken }
	return max(w, h) <= budgetMaxSide && tiles(w)*tiles(h) <= budgetMaxTokens
}

// scaleLongestSide scales w x h so its longest side is long, preserving the aspect ratio with integer rounding and at
// least one pixel per axis.
func scaleLongestSide(w, h, long int) (int, int) {
	if w >= h {
		return long, max(1, int(math.Round(float64(h)*float64(long)/float64(w))))
	}
	return max(1, int(math.Round(float64(w)*float64(long)/float64(h)))), long
}

// fitTokenBudget returns w x h unchanged when it is within the token budget, else the largest size of the same aspect
// ratio within it. It binary-searches the longest side (searching the width would fail for a 1-pixel-wide tall
// image); both tile counts grow with it, and a longest side of 1 always fits.
func fitTokenBudget(w, h int) (int, int) {
	if withinTokenBudget(w, h) {
		return w, h
	}
	lo, hi := 1, min(max(w, h), budgetMaxSide)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if withinTokenBudget(scaleLongestSide(w, h, mid)) {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return scaleLongestSide(w, h, lo)
}

// clampRegion intersects a window rect (screen pixels) with a w x h screenshot. ok is false when nothing of the rect
// is on the image (a minimized window sits at -32000,-32000), and the region is then empty.
func clampRegion(r proto.Rect, w, h int) (region screenshotRegion, ok bool) {
	x0, y0 := max(int(r.Left), 0), max(int(r.Top), 0)
	x1, y1 := min(int(r.Right), w), min(int(r.Bottom), h)
	if x1 <= x0 || y1 <= y0 {
		return screenshotRegion{}, false
	}
	return screenshotRegion{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0}, true
}

// screenshotGeometry uses original screenshot pixels for the crop and output/crop ratios for scale.
type screenshotGeometry struct {
	OriginalWidth  int     `json:"original_width"`
	OriginalHeight int     `json:"original_height"`
	X              int     `json:"x"`
	Y              int     `json:"y"`
	Width          int     `json:"width"`
	Height         int     `json:"height"`
	OutputWidth    int     `json:"output_width"`
	OutputHeight   int     `json:"output_height"`
	ScaleX         float64 `json:"scale_x"`
	ScaleY         float64 `json:"scale_y"`
}

func transformScreenshot(data []byte, opts screenshotOptions) ([]byte, screenshotGeometry, error) {
	var g screenshotGeometry
	if opts.MaxSize < -1 {
		return nil, g, fmt.Errorf("max_size must be -1, 0 or positive")
	}
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, g, fmt.Errorf("decode screenshot: %w", err)
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	r := screenshotRegion{Width: w, Height: h}
	if opts.Region != nil {
		r = *opts.Region
	}
	// Subtraction after checking the origin avoids overflowing x+width or y+height.
	if r.X < 0 || r.Y < 0 || r.Width <= 0 || r.Height <= 0 || r.X > w || r.Y > h || r.Width > w-r.X || r.Height > h-r.Y {
		return nil, g, fmt.Errorf("region must have positive dimensions and fit within the %dx%d screenshot", w, h)
	}
	ow, oh := r.Width, r.Height
	if opts.MaxSize == 0 {
		ow, oh = fitTokenBudget(ow, oh)
	} else if opts.MaxSize > 0 && max(ow, oh) > opts.MaxSize {
		ow, oh = scaleLongestSide(ow, oh, opts.MaxSize)
	}
	g = screenshotGeometry{
		OriginalWidth: w, OriginalHeight: h,
		X: r.X, Y: r.Y, Width: r.Width, Height: r.Height,
		OutputWidth: ow, OutputHeight: oh,
		ScaleX: float64(ow) / float64(r.Width), ScaleY: float64(oh) / float64(r.Height),
	}
	if r.X == 0 && r.Y == 0 && r.Width == w && r.Height == h && ow == w && oh == h {
		return data, g, nil
	}
	dst := image.NewNRGBA(image.Rect(0, 0, ow, oh))
	if ow == r.Width && oh == r.Height {
		draw.Draw(dst, dst.Bounds(), src, image.Pt(r.X, r.Y), draw.Src)
	} else {
		// Nearest-neighbour sampling at pixel centres preserves exact source colours without another dependency.
		for y := 0; y < oh; y++ {
			sy := r.Y + min(r.Height-1, int((float64(y)+0.5)*float64(r.Height)/float64(oh)))
			for x := 0; x < ow; x++ {
				sx := r.X + min(r.Width-1, int((float64(x)+0.5)*float64(r.Width)/float64(ow)))
				dst.Set(x, y, src.At(sx, sy))
			}
		}
	}
	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, g, fmt.Errorf("encode screenshot: %w", err)
	}
	return out.Bytes(), g, nil
}
