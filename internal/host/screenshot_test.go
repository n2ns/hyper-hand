package host

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"testing"
)

func screenshotFixture(t *testing.T, w, h int) ([]byte, *image.NRGBA) {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.SetNRGBA(x, y, color.NRGBA{R: uint8(x * 20), G: uint8(y * 30), B: 100, A: 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, im); err != nil {
		t.Fatal(err)
	}
	return b.Bytes(), im
}

func TestTransformScreenshotUnchanged(t *testing.T) {
	data, _ := screenshotFixture(t, 7, 5)
	for _, opts := range []screenshotOptions{
		{}, {MaxSize: 7}, {MaxSize: 20},
		{Region: &screenshotRegion{Width: 7, Height: 5}},
	} {
		got, g, err := transformScreenshot(data, opts)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("untransformed screenshot changed bytes")
		}
		want := screenshotGeometry{OriginalWidth: 7, OriginalHeight: 5, Width: 7, Height: 5, OutputWidth: 7, OutputHeight: 5, ScaleX: 1, ScaleY: 1}
		if g != want {
			t.Fatalf("geometry = %+v, want %+v", g, want)
		}
	}
}

func TestTransformScreenshotCropPixels(t *testing.T) {
	data, src := screenshotFixture(t, 7, 5)
	got, g, err := transformScreenshot(data, screenshotOptions{Region: &screenshotRegion{X: 2, Y: 1, Width: 5, Height: 4}})
	if err != nil {
		t.Fatal(err)
	}
	want := screenshotGeometry{OriginalWidth: 7, OriginalHeight: 5, X: 2, Y: 1, Width: 5, Height: 4, OutputWidth: 5, OutputHeight: 4, ScaleX: 1, ScaleY: 1}
	if g != want {
		t.Fatalf("geometry = %+v, want %+v", g, want)
	}
	im, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	if im.Bounds() != image.Rect(0, 0, 5, 4) {
		t.Fatal(im.Bounds())
	}
	for y := 0; y < 4; y++ {
		for x := 0; x < 5; x++ {
			if color.NRGBAModel.Convert(im.At(x, y)) != src.At(x+2, y+1) {
				t.Fatalf("wrong cropped pixel at %d,%d", x, y)
			}
		}
	}
}

func TestTransformScreenshotScaleGeometry(t *testing.T) {
	for _, tt := range []struct{ w, h, limit, ow, oh int }{
		{7, 5, 4, 4, 3}, {5, 7, 4, 3, 4}, {7, 1, 1, 1, 1},
		{1, 7, 1, 1, 1}, {4, 4, 2, 2, 2},
	} {
		data, _ := screenshotFixture(t, tt.w, tt.h)
		got, g, err := transformScreenshot(data, screenshotOptions{MaxSize: tt.limit})
		if err != nil {
			t.Fatal(err)
		}
		if g.OutputWidth != tt.ow || g.OutputHeight != tt.oh || g.ScaleX != float64(tt.ow)/float64(tt.w) || g.ScaleY != float64(tt.oh)/float64(tt.h) {
			t.Fatalf("%+v: geometry %+v", tt, g)
		}
		im, err := png.Decode(bytes.NewReader(got))
		if err != nil || im.Bounds() != image.Rect(0, 0, tt.ow, tt.oh) {
			t.Fatalf("output dimensions: %v, %v", im, err)
		}
	}
}

func TestTransformScreenshotCropAndScalePixels(t *testing.T) {
	data, src := screenshotFixture(t, 8, 6)
	got, g, err := transformScreenshot(data, screenshotOptions{Region: &screenshotRegion{X: 2, Y: 1, Width: 4, Height: 4}, MaxSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if g.X != 2 || g.Y != 1 || g.Width != 4 || g.Height != 4 || g.ScaleX != 0.5 || g.ScaleY != 0.5 {
		t.Fatalf("geometry %+v", g)
	}
	im, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	// Each output pixel samples the centre of its 2x2 source area, after the crop offset.
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			if color.NRGBAModel.Convert(im.At(x, y)) != src.At(3+2*x, 2+2*y) {
				t.Fatalf("wrong scaled pixel at %d,%d", x, y)
			}
		}
	}
}

func TestTransformScreenshotInvalid(t *testing.T) {
	data, _ := screenshotFixture(t, 7, 5)
	for _, r := range []screenshotRegion{
		{}, {Width: 0, Height: 1}, {Width: 1, Height: -1},
		{X: -1, Width: 1, Height: 1}, {Y: -1, Width: 1, Height: 1},
		{X: 7, Width: 1, Height: 1}, {Y: 5, Width: 1, Height: 1},
		{X: 1, Width: 7, Height: 1}, {Y: 1, Width: 1, Height: 5},
		{X: math.MaxInt, Width: math.MaxInt, Height: 1},
		{Y: 1, Width: 1, Height: math.MaxInt},
	} {
		if _, _, err := transformScreenshot(data, screenshotOptions{Region: &r}); err == nil {
			t.Fatalf("accepted invalid region %+v", r)
		}
	}
	if _, _, err := transformScreenshot(data, screenshotOptions{MaxSize: -2}); err == nil {
		t.Fatal("accepted max_size -2")
	}
	for _, invalid := range [][]byte{nil, {}, []byte("not a PNG"), data[:len(data)/2]} {
		if _, _, err := transformScreenshot(invalid, screenshotOptions{}); err == nil {
			t.Fatal("accepted invalid PNG")
		}
	}
}

func TestFitTokenBudget(t *testing.T) {
	for _, tt := range []struct{ w, h, ow, oh int }{
		{1920, 1080, 1456, 819}, // 52x30 = 1560 tiles; a longest side of 1457 needs 53x30
		{2560, 1600, 1389, 868}, // 50x31 = 1550 tiles
		{1900, 1040, 1484, 812}, // 53x29 = 1537 tiles
		{1280, 720, 1280, 720},  // 46x26 = 1196 tiles: within the budget, unchanged
		{800, 600, 800, 600},    // small window, unchanged
		{1568, 28, 1568, 28},    // exactly at the side limit
		{400, 1900, 330, 1568},  // narrow tall window: only the longest side limits it
		{1, 5000, 1, 1568},      // 1xN keeps one pixel on the short axis
		{5000, 1, 1568, 1},
		{1, 1, 1, 1},
	} {
		ow, oh := fitTokenBudget(tt.w, tt.h)
		if ow != tt.ow || oh != tt.oh {
			t.Errorf("%dx%d: got %dx%d, want %dx%d", tt.w, tt.h, ow, oh, tt.ow, tt.oh)
		}
		if !withinTokenBudget(ow, oh) {
			t.Errorf("%dx%d: %dx%d exceeds the budget", tt.w, tt.h, ow, oh)
		}
		// Largest such size: one more pixel on the longest side would exceed the budget.
		if long := max(ow, oh); long < max(tt.w, tt.h) && withinTokenBudget(scaleLongestSide(tt.w, tt.h, long+1)) {
			t.Errorf("%dx%d: %dx%d is not the largest size within the budget", tt.w, tt.h, ow, oh)
		}
	}
}

func TestTransformScreenshotSizeModes(t *testing.T) {
	data, _ := screenshotFixture(t, 1920, 1080)
	for _, tt := range []struct {
		name    string
		opts    screenshotOptions
		ow, oh  int
		samePNG bool
	}{
		{"default budget", screenshotOptions{}, 1456, 819, false},
		{"original", screenshotOptions{MaxSize: -1}, 1920, 1080, true},
		{"longest side", screenshotOptions{MaxSize: 1000}, 1000, 563, false},
		{"longest side above the budget", screenshotOptions{MaxSize: 1600}, 1600, 900, false},
		{"window within the budget", screenshotOptions{Region: &screenshotRegion{X: 100, Y: 50, Width: 600, Height: 400}}, 600, 400, false},
		{"window over the budget", screenshotOptions{Region: &screenshotRegion{X: 10, Y: 0, Width: 1900, Height: 1040}}, 1484, 812, false},
		{"window original", screenshotOptions{Region: &screenshotRegion{X: 10, Y: 0, Width: 1900, Height: 1040}, MaxSize: -1}, 1900, 1040, false},
	} {
		got, g, err := transformScreenshot(data, tt.opts)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if g.OutputWidth != tt.ow || g.OutputHeight != tt.oh || g.ScaleX != float64(tt.ow)/float64(g.Width) || g.ScaleY != float64(tt.oh)/float64(g.Height) {
			t.Errorf("%s: geometry %+v, want %dx%d", tt.name, g, tt.ow, tt.oh)
		}
		if same := bytes.Equal(got, data); same != tt.samePNG {
			t.Errorf("%s: original PNG bytes kept = %v", tt.name, same)
		}
		im, err := png.Decode(bytes.NewReader(got))
		if err != nil || im.Bounds() != image.Rect(0, 0, tt.ow, tt.oh) {
			t.Errorf("%s: output %v, %v", tt.name, im.Bounds(), err)
		}
	}
}
