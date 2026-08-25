// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPixelFormat(t *testing.T) {
	if FormatRGBA.String() != "RGBA_8888" {
		t.Errorf("FormatRGBA.String() = %q", FormatRGBA.String())
	}
	if FormatRGBA.BytesPerPixel() != 4 {
		t.Errorf("FormatRGBA.BytesPerPixel() = %d", FormatRGBA.BytesPerPixel())
	}
	if got := PixelFormat(42).String(); got != "PixelFormat(42)" {
		t.Errorf("unknown format = %q", got)
	}
	if PixelFormat(42).BytesPerPixel() != 0 {
		t.Error("an unknown format claimed a pixel size")
	}
}

func TestDisplayDescribesItself(t *testing.T) {
	d := Display{ID: 0, Name: "Built-in Screen", Width: 1080, Height: 2400,
		DensityDPI: 420, RefreshRate: 60}
	if !d.Default() || d.Presentation() {
		t.Errorf("built-in: Default=%v Presentation=%v", d.Default(), d.Presentation())
	}
	s := d.String()
	for _, want := range []string{"Built-in Screen", "1080x2400", "420dpi", "(default)"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() = %q, missing %q", s, want)
		}
	}
	// A headset on DP Alt Mode: not the default, and flagged for an
	// application's own full-screen content.
	g := Display{ID: 3, Name: "VITURE Beast", Width: 3840, Height: 1080,
		DensityDPI: 320, RefreshRate: 60, Flags: FlagPresentation}
	if g.Default() || !g.Presentation() {
		t.Errorf("glasses: Default=%v Presentation=%v", g.Default(), g.Presentation())
	}
	if !strings.Contains(g.String(), "(presentation)") || strings.Contains(g.String(), "(default)") {
		t.Errorf("String() = %q", g.String())
	}
}

func TestOptionsValidate(t *testing.T) {
	for name, o := range map[string]Options{
		"negative size":  {Width: -1, Height: -1},
		"half a size":    {Width: 100},
		"absurd size":    {Width: MaxDimension + 1, Height: 1},
		"negative fps":   {FPS: -1},
		"sub-hertz fps":  {FPS: 0.001},
		"negative depth": {QueueDepth: -1},
		"shallow depth":  {QueueDepth: 2},
		"deep depth":     {QueueDepth: MaxQueueDepth + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if err := o.Validate(); !errors.Is(err, ErrInvalidOption) {
				t.Errorf("Validate(%+v) = %v, want ErrInvalidOption", o, err)
			}
		})
	}
	for name, o := range map[string]Options{
		"zero":     {},
		"explicit": {Width: 1080, Height: 2400, FPS: 30, QueueDepth: 4},
		"slow":     {FPS: 0.01},
	} {
		t.Run(name, func(t *testing.T) {
			if err := o.Validate(); err != nil {
				t.Errorf("Validate(%+v) = %v", o, err)
			}
		})
	}
}

func TestOptionsResolve(t *testing.T) {
	got, err := Options{}.resolve(1080, 2400)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Width != 1080 || got.Height != 2400 || got.FPS != DefaultFPS || got.QueueDepth != DefaultQueueDepth {
		t.Errorf("resolve = %+v", got)
	}
	// An explicit size survives untouched.
	got, err = Options{Width: 640, Height: 480, FPS: 15, QueueDepth: 5}.resolve(1080, 2400)
	if err != nil || got.Width != 640 || got.FPS != 15 || got.QueueDepth != 5 {
		t.Errorf("resolve of an explicit size = %+v, %v", got, err)
	}
	_, err = (Options{QueueDepth: 2}).resolve(1080, 2400)
	if !errors.Is(err, ErrInvalidOption) {
		t.Errorf("resolve validated nothing: %v", err)
	}
	_, err = (Options{}).resolve(0, 0)
	if !errors.Is(err, ErrInvalidOption) {
		t.Errorf("resolve of a sizeless display = %v, want ErrInvalidOption", err)
	}
}

func TestMilliFPS(t *testing.T) {
	if got := milliFPS(60); got != 60000 {
		t.Errorf("milliFPS(60) = %d", got)
	}
	if got := milliFPS(0); got != uint32(DefaultFPS*1000) {
		t.Errorf("milliFPS(0) = %d, want the default rather than 'as fast as possible'", got)
	}
	if got := milliFPS(-5); got != uint32(DefaultFPS*1000) {
		t.Errorf("milliFPS(-5) = %d", got)
	}
	if got := milliFPS(0.0001); got != 1 {
		t.Errorf("milliFPS(0.0001) = %d, want a clamp to 1 rather than 0", got)
	}
	if got := milliFPS(1e12); got != ^uint32(0) {
		t.Errorf("milliFPS(1e12) = %d, want a clamp rather than an overflow", got)
	}
}

// frame builds a padded frame: stride is deliberately larger than Width*4,
// because trusting Width*4 is the single most common way to get a sheared
// image out of a capture API.
func frame(w, h, pad int) Frame {
	stride := w*4 + pad
	pix := make([]byte, stride*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			pix[y*stride+x*4+0] = byte(x)
			pix[y*stride+x*4+1] = byte(y)
			pix[y*stride+x*4+2] = 0x30
			pix[y*stride+x*4+3] = 0xff
		}
		for p := w * 4; p < stride; p++ {
			pix[y*stride+p] = 0xee // padding, which must never be copied out
		}
	}
	return Frame{Pix: pix, Width: w, Height: h, Stride: stride, Seq: 1, At: time.Unix(1, 0)}
}

func TestFrameValidity(t *testing.T) {
	if !frame(4, 3, 8).Valid() {
		t.Error("a well-formed frame was invalid")
	}
	for name, f := range map[string]Frame{
		"empty":        {},
		"no width":     {Pix: make([]byte, 16), Height: 1, Stride: 16},
		"no height":    {Pix: make([]byte, 16), Width: 4, Stride: 16},
		"short stride": {Pix: make([]byte, 16), Width: 4, Height: 1, Stride: 8},
		"short pix":    {Pix: make([]byte, 8), Width: 4, Height: 2, Stride: 16},
	} {
		t.Run(name, func(t *testing.T) {
			if f.Valid() {
				t.Errorf("%+v claimed to be valid", f)
			}
		})
	}
}

func TestFrameRowTrimsThePadding(t *testing.T) {
	f := frame(4, 3, 8)
	if f.TightLen() != 4*4*3 {
		t.Errorf("TightLen = %d", f.TightLen())
	}
	row := f.Row(1)
	if len(row) != 16 {
		t.Fatalf("Row(1) is %d bytes, want 16 — the padding came with it", len(row))
	}
	if row[0] != 0 || row[1] != 1 || row[4] != 1 {
		t.Errorf("Row(1) = %v", row[:8])
	}
	// The returned row must not be able to reach the padding through append.
	if got := cap(row); got != 16 {
		t.Errorf("Row cap = %d, want 16 so an append cannot scribble on the next row", got)
	}
	if f.Row(-1) != nil || f.Row(3) != nil {
		t.Error("Row accepted an out-of-range y")
	}
	if (Frame{}).Row(0) != nil {
		t.Error("Row on an invalid frame returned bytes")
	}
}

func TestFrameCopyTight(t *testing.T) {
	f := frame(4, 3, 8)
	dst := make([]byte, f.TightLen())
	n, err := f.CopyTight(dst)
	if err != nil || n != len(dst) {
		t.Fatalf("CopyTight = %d, %v", n, err)
	}
	for _, b := range dst {
		if b == 0xee {
			t.Fatal("the row padding was copied out — this is exactly the sheared-image bug")
		}
	}
	if dst[4*4+4] != 1 { // row 1, pixel 1, green channel = y
		t.Errorf("row 1 landed wrong: %v", dst[16:24])
	}

	// The unpadded fast path.
	g := frame(4, 3, 0)
	n, err = g.CopyTight(dst)
	if err != nil || n != len(dst) {
		t.Fatalf("CopyTight of an unpadded frame = %d, %v", n, err)
	}

	if _, err := f.CopyTight(make([]byte, 4)); !errors.Is(err, ErrShortBuffer) {
		t.Errorf("CopyTight into a small buffer = %v, want ErrShortBuffer", err)
	}
	if _, err := (Frame{}).CopyTight(dst); !errors.Is(err, ErrNoFrame) {
		t.Errorf("CopyTight of an invalid frame = %v, want ErrNoFrame", err)
	}
}

func TestFrameNRGBA(t *testing.T) {
	f := frame(4, 3, 8)
	img, err := f.NRGBA()
	if err != nil {
		t.Fatalf("NRGBA: %v", err)
	}
	if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 3 {
		t.Fatalf("NRGBA bounds = %v", img.Bounds())
	}
	// Android hands back RGBA_8888, which IS image.NRGBA's order: no swap.
	c := img.NRGBAAt(1, 2)
	if c.R != 1 || c.G != 2 || c.B != 0x30 || c.A != 0xff {
		t.Errorf("pixel (1,2) = %+v, want R=1 G=2 B=0x30", c)
	}
	if _, err := (Frame{}).NRGBA(); !errors.Is(err, ErrNoFrame) {
		t.Errorf("NRGBA of an invalid frame = %v, want ErrNoFrame", err)
	}
}

func TestStatsFPS(t *testing.T) {
	if (Stats{}).FPS() != 0 {
		t.Error("FPS with no frames was not zero")
	}
	if got := (Stats{Interval: 20 * time.Millisecond}).FPS(); got < 49.9 || got > 50.1 {
		t.Errorf("FPS = %v, want 50", got)
	}
}

// TestErrorsCarryTheirRemedy: a sentinel whose message does not say what to do
// is a worse error than no sentinel at all.
func TestErrorsCarryTheirRemedy(t *testing.T) {
	if !strings.Contains(ErrPermissionDenied.Error(), "RequestAuthorization") {
		t.Errorf("ErrPermissionDenied = %q", ErrPermissionDenied)
	}
	if !strings.Contains(ErrNotCapturable.Error(), "CAPTURE_VIDEO_OUTPUT") {
		t.Errorf("ErrNotCapturable = %q", ErrNotCapturable)
	}
	if !strings.Contains(ErrUnsupported.Error(), "host") {
		t.Errorf("ErrUnsupported = %q", ErrUnsupported)
	}
}
