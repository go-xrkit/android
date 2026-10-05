// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrwide measures how WIDE a display this application owns can be, on
// the device it is running on, and whether the pixels actually arrive at that
// width.
//
// It exists because the width is the whole point. go-xrkit/desk puts a 6400
// pixel spreadsheet in front of somebody on macOS; the Android ribbon can only
// do the same if an owned display can BE that wide. [android.MaxDimension] says
// 32768, which is the package's own guard rather than a measurement of any
// particular phone -- a Pixel is not obliged to honour it, and a width that is
// accepted and then hands back a black buffer is the silent failure this whole
// mechanism has to be proved against.
//
//	APP=./cmd/xrwide host/build.sh && adb install -r host/out/xrhost.apk
//	adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity --ez capture false
//	adb logcat -s xrwide
//
// ⛔ IT SWEEPS RATHER THAN ASKING FOR ONE NUMBER. A single width answers "did
// that work" and nothing about where the edge is; the sweep names the last
// width that carried its pixels and the first that did not, which is the only
// form in which this is worth writing down.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-xrkit/android"
)

func main() { os.Exit(run(context.Background())) }

// widths are the ones worth knowing about, each for a reason: the desk's own
// panel, a doubled one, the width a wide spreadsheet is actually used at on
// macOS, and then past the point where a phone might reasonably stop.
var widths = []int{1920, 3840, 6400, 8640, 16384, 32768}

func run(ctx context.Context) int {
	fmt.Printf("DEVICE MaxDimension guard is %d\n", android.MaxDimension)

	widest, firstRefused, firstBlack := 0, 0, 0
	for _, w := range widths {
		// One wall per width, closed before the next: a display left open
		// holds memory proportional to its area, and 32768x1080 is 135 MB of
		// BGRA on its own. Measuring the edge must not be what pushes the
		// process over it.
		wall, err := android.NewWall(1)
		if err != nil {
			fmt.Printf("FAIL NewWall for %d: %v\n", w, err)
			return 1
		}
		ok, black := tryWidth(ctx, wall, w)
		_ = wall.Close()
		switch {
		case ok:
			widest = w
		case black:
			if firstBlack == 0 {
				firstBlack = w
			}
		default:
			if firstRefused == 0 {
				firstRefused = w
			}
		}
	}

	fmt.Printf("RESULT widest display that carried its pixels: %d\n", widest)
	if firstRefused != 0 {
		fmt.Printf("RESULT first width REFUSED outright: %d\n", firstRefused)
	}
	// ⛔ A WIDTH THAT IS ACCEPTED AND COMES BACK BLACK IS THE WORST ANSWER, and
	// it is the one a naive check reports as success. It is named separately
	// from a refusal because the remedy is different: a refusal is a limit to
	// respect, a black buffer is a limit that LIES.
	if firstBlack != 0 {
		fmt.Printf("RESULT first width ACCEPTED BUT BLANK: %d -- "+
			"accepted and handed back no pixels\n", firstBlack)
		return 1
	}
	if widest == 0 {
		fmt.Println("RESULT no width worked at all")
		return 1
	}
	return 0
}

// tryWidth opens one display of that width and says whether its pixels arrived.
// The second return distinguishes "refused" from "accepted and blank".
func tryWidth(ctx context.Context, wall *android.Wall, w int) (ok, black bool) {
	const h = 1080
	d, err := wall.Open(ctx, android.DisplaySpec{
		Width: w, Height: h, Content: android.Sentinel{Label: fmt.Sprintf("%d", w)},
	})
	if err != nil {
		fmt.Printf("WIDTH %-6d refused: %v\n", w, err)
		return false, false
	}
	defer func() { _ = d.Close() }()
	fmt.Printf("WIDTH %-6d opened: %s\n", w, d)

	// A wide surface takes longer to compose than a 640x480 one, so the wait is
	// generous rather than the 15s xrwall uses -- a timeout here would be read
	// as "the width does not work", which is a different claim.
	wctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	f, err := d.WaitFrame(wctx)
	if err != nil {
		fmt.Printf("WIDTH %-6d opened but produced NO FRAME: %v\n", w, err)
		return false, true
	}
	if f.Width != w {
		fmt.Printf("WIDTH %-6d came back %dx%d -- the platform resized it\n",
			w, f.Width, f.Height)
	}

	// Sampled at the coordinates SentinelColorAt vouches for, exactly as
	// xrwall does, so the two commands cannot disagree about what a correct
	// panel looks like.
	blackPx, wrong, checked := 0, 0, 0
	for y := 0; y < f.Height; y++ {
		row := f.Row(y)
		for x := 0; x < f.Width; x++ {
			px := uint32(row[x*4])<<16 | uint32(row[x*4+1])<<8 | uint32(row[x*4+2])
			if px == 0 {
				blackPx++
			}
			want, vouched := android.SentinelColorAt(f.Width, f.Height, x, y)
			if !vouched {
				continue
			}
			checked++
			if px != want {
				wrong++
			}
		}
	}
	total := f.Width * f.Height
	fmt.Printf("WIDTH %-6d %dx%d: sampled %d, wrong %d, black %d/%d\n",
		w, f.Width, f.Height, checked, wrong, blackPx, total)
	if checked == 0 {
		fmt.Printf("WIDTH %-6d nothing was sampled; the check read no pixels\n", w)
		return false, true
	}
	if blackPx == total {
		return false, true
	}
	return wrong == 0, false
}
