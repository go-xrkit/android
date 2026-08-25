// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrwall proves, on a real device, that this application can create
// displays of its own, render Android content on them and read the pixels back
// — and that the limit that keeps it from rebooting the phone actually refuses.
//
// It is packaged into the APK by host/build.sh and reports to logcat:
//
//	APP=./cmd/xrwall host/build.sh && adb install -r host/out/xrhost.apk
//	adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity
//	adb logcat -s xrwall xr-wall
//
// Nothing here looks at a picture. Every claim is asserted on SAMPLED PIXELS at
// coordinates [android.SentinelColorAt] vouches for, because a feed that
// "works" and hands back a black buffer is the silent failure this whole
// mechanism has to be proved against.
package main

import (
	"context"
	"errors"
	"fmt"
	"image/png"
	"os"
	"path/filepath"
	"time"

	"github.com/go-xrkit/android"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	fmt.Printf("WALL available=%v\n", android.WallAvailable())
	fmt.Printf("WALL package default %d, ceiling %d\n",
		android.DefaultMaxDisplays, android.MaxDisplays)

	bad := 0
	w, err := android.NewWall(0) // the package default
	if err != nil {
		fmt.Printf("FAIL NewWall: %v\n", err)
		return 1
	}
	defer func() { _ = w.Close() }()

	const panels = 3
	var first *android.OwnedDisplay
	for i := 0; i < panels; i++ {
		d, err := w.Open(ctx, android.DisplaySpec{
			Width: 640, Height: 480,
			Content: android.Sentinel{Label: fmt.Sprintf("%d", i)},
		})
		if err != nil {
			fmt.Printf("FAIL Open %d: %v\n", i, err)
			return 1
		}
		fmt.Printf("OPEN %s\n", d)
		if !checkSentinel(ctx, d, i) {
			bad++
		}
		if first == nil {
			first = d
		}
	}
	fmt.Printf("WALL holds %d of %d\n", w.Len(), w.Max())

	// A WebView on a display we made: the content a CGO-free Go process cannot
	// render itself, which is the whole reason to route a panel through a real
	// Android display rather than compositing in Go.
	if !checkWeb(ctx, w) {
		bad++
	}
	if first != nil {
		save(first)
	}

	if !checkLimit(ctx) {
		bad++
	}
	if bad != 0 {
		fmt.Printf("RESULT %d check(s) FAILED\n", bad)
		return 1
	}
	fmt.Println("RESULT every panel carried the pixels its content drew, " +
		"and the limit refused in both directions")
	return 0
}

// checkLimit proves the refusal in BOTH directions on a wall of its own:
// exactly the limit must succeed, one more must fail with the named error, and
// the failure must create nothing.
func checkLimit(ctx context.Context) bool {
	const max = 2
	w, err := android.NewWall(max)
	if err != nil {
		fmt.Printf("FAIL NewWall(%d): %v\n", max, err)
		return false
	}
	defer func() { _ = w.Close() }()

	for i := 0; i < max; i++ {
		if _, err := w.Open(ctx, android.DisplaySpec{
			Width: 320, Height: 240, Content: android.Sentinel{},
		}); err != nil {
			fmt.Printf("FAIL Open %d of %d, which is within the limit: %v\n", i+1, max, err)
			return false
		}
	}
	fmt.Printf("LIMIT %d of %d opened, as they must\n", w.Len(), max)

	extra, err := w.Open(ctx, android.DisplaySpec{
		Width: 320, Height: 240, Content: android.Sentinel{},
	})
	switch {
	case extra != nil:
		fmt.Println("FAIL Open past the limit returned a display")
		return false
	case !errors.Is(err, android.ErrTooManyDisplays):
		fmt.Printf("FAIL Open past the limit: %v, want ErrTooManyDisplays\n", err)
		return false
	}
	fmt.Printf("LIMIT refused past %d, as it must: %v\n", max, err)
	if w.Len() != max {
		fmt.Printf("FAIL a refusal changed the wall: %d displays, want %d\n", w.Len(), max)
		return false
	}

	// And NewWall itself refuses a limit past the package ceiling.
	if _, err := android.NewWall(android.MaxDisplays + 1); !errors.Is(err, android.ErrTooManyDisplays) {
		fmt.Printf("FAIL NewWall(%d): %v, want ErrTooManyDisplays\n", android.MaxDisplays+1, err)
		return false
	}
	fmt.Printf("LIMIT NewWall refuses past the ceiling of %d, as it must\n", android.MaxDisplays)
	return true
}

// checkSentinel asserts a frame arrived and that it is the one the Presentation
// drew, by SAMPLING points whose colour is known in advance.
func checkSentinel(ctx context.Context, d *android.OwnedDisplay, i int) bool {
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f, err := d.WaitFrame(wctx)
	if err != nil {
		fmt.Printf("FAIL panel %d produced no frame: %v\n", i, err)
		return false
	}
	black, wrong, checked := 0, 0, 0
	var firstWrong string
	for y := 0; y < f.Height; y++ {
		row := f.Row(y)
		for x := 0; x < f.Width; x++ {
			px := uint32(row[x*4])<<16 | uint32(row[x*4+1])<<8 | uint32(row[x*4+2])
			if px == 0 {
				black++
			}
			want, ok := android.SentinelColorAt(f.Width, f.Height, x, y)
			if !ok {
				continue
			}
			checked++
			if px != want {
				wrong++
				if firstWrong == "" {
					firstWrong = fmt.Sprintf("(%d,%d)=#%06X want #%06X", x, y, px, want)
				}
			}
		}
	}
	fmt.Printf("FRAME panel %d %dx%d stride %d seq %d: sampled %d, wrong %d, black %d/%d\n",
		i, f.Width, f.Height, f.Stride, f.Seq, checked, wrong, black, f.Width*f.Height)
	if wrong != 0 {
		fmt.Printf("FAIL panel %d carries pixels the Presentation did not draw: %s\n", i, firstWrong)
		return false
	}
	if black == f.Width*f.Height {
		fmt.Printf("FAIL panel %d is entirely black — the silent failure\n", i)
		return false
	}
	return true
}

// checkWeb renders a page whose background is a colour nothing else here uses,
// so "the WebView drew" is a pixel test rather than an absence of errors.
func checkWeb(ctx context.Context, w *android.Wall) bool {
	const url = "data:text/html," +
		"<body style='margin:0;background:%23FFFF00'><h1>go-xrkit</h1></body>"
	d, err := w.Open(ctx, android.DisplaySpec{Width: 640, Height: 480, Content: android.Web{URL: url}})
	if err != nil {
		fmt.Printf("FAIL Open a web panel: %v\n", err)
		return false
	}
	defer func() { _ = d.Close() }()
	fmt.Printf("OPEN %s\n", d)

	// A WebView lays out and paints asynchronously; take frames until one is
	// mostly the page's own yellow, or give up and say so.
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	best := 0.0
	for wctx.Err() == nil {
		f, err := d.WaitFrame(wctx)
		if err != nil {
			break
		}
		yellow := 0
		for y := 0; y < f.Height; y++ {
			row := f.Row(y)
			for x := 0; x < f.Width; x++ {
				if row[x*4] > 0xE0 && row[x*4+1] > 0xE0 && row[x*4+2] < 0x40 {
					yellow++
				}
			}
		}
		frac := float64(yellow) / float64(f.Width*f.Height)
		if frac > best {
			best = frac
		}
		if frac > 0.5 {
			fmt.Printf("FRAME web panel %dx%d seq %d: %.1f%% of the pixels are the page's own colour\n",
				f.Width, f.Height, f.Seq, frac*100)
			return true
		}
	}
	fmt.Printf("FAIL the web panel never rendered its page (best %.1f%% yellow)\n", best*100)
	return false
}

// save writes one frame as a PNG, somewhere it cannot be committed.
//
// The directory is walked up to the filesystem root looking for a .git and the
// write is refused if one is found. On a device the working directory is the
// app's external files directory and no repository is anywhere near it, but the
// check is the fleet rule and a rule that is only applied where it cannot fail
// is not a rule.
func save(d *android.OwnedDisplay) {
	// Frame's second value is FRESHNESS, not validity, and the frame this
	// panel produced has already been consumed by WaitFrame. What matters here
	// is that there are pixels at all, which is what Valid answers.
	f, _ := d.Frame()
	if !f.Valid() {
		fmt.Println("FAIL no frame to save")
		return
	}
	img, err := f.NRGBA()
	if err != nil {
		fmt.Printf("FAIL converting the frame: %v\n", err)
		return
	}
	dir, err := artifactDir()
	if err != nil {
		fmt.Printf("FAIL choosing a directory for the capture: %v\n", err)
		return
	}
	path := filepath.Join(dir, "wall-sentinel.png")
	out, err := os.Create(path)
	if err != nil {
		fmt.Printf("FAIL creating %s: %v\n", path, err)
		return
	}
	defer func() { _ = out.Close() }()
	if err := png.Encode(out, img); err != nil {
		fmt.Printf("FAIL encoding %s: %v\n", path, err)
		return
	}
	fmt.Printf("ARTIFACT %s\n", path)
}

// ErrInRepository is reported when the chosen artefact directory is inside a
// git work tree.
var errInRepository = errors.New("a capture must never be written where it can be committed")

func artifactDir() (string, error) {
	dir := os.Getenv("XRKIT_ARTIFACT_DIR")
	if dir == "" {
		// On a device the host sets the working directory to the app's external
		// files directory, which is inside no repository and survives the run.
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for d := abs; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return "", fmt.Errorf("%w: %s is inside the work tree at %s", errInRepository, abs, d)
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return abs, os.MkdirAll(abs, 0o755)
}
