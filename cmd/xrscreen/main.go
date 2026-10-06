// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrscreen paints frames from Go onto the glasses and says how many of
// them Android took.
//
// It is the OUTPUT half of this package, and the only one that cannot be proved
// from a cable. cmd/xrwide and cmd/xrwall read back what Android rendered and
// sample the pixels; there is nothing to read back here, because an ordinary
// application may not capture a display it does not own. The glasses are the
// only instrument, and the person wearing them is the only witness.
//
//	APP=./cmd/xrscreen host/build.sh && adb install -r host/out/xrhost.apk
//	adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity --ez capture false
//	adb logcat -d -s xrcapture xr-wall
//	adb shell run-as org.goxrkit.androidhost cat files/screen.txt
//
// ⛔ AN ACKNOWLEDGEMENT IS NOT LIGHT. Every frame this reports was copied into
// the host's bitmap and handed to the view — that is what the count means, and
// it is worth having, because it separates "the mechanism works" from "the
// mechanism runs and shows black", which is this whole feature's silent
// failure. It does NOT say a photon left the panel. What is drawn is therefore
// deliberately unmistakable — flat quadrants of known colour and a bar that
// sweeps — so that ONE GLANCE through the glasses settles the rest.
//
// ⚠ THE BAR IS NOT DECORATION. Colour alone refutes a black panel. The bar
// refutes a screen FROZEN ON ITS FIRST FRAME, which is what a host that showed
// one bitmap and then stopped copying looks like: 3600 acknowledgements and one
// picture. The two failures need two different things to look at, so a witness
// has to be asked about both — and on the Pixel 11 Pro Fold with VITURE Beast
// glasses, both were reported: colour, and movement.
package main

import (
	"context"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"strings"
	"time"

	"github.com/go-xrkit/android"
)

func main() { os.Exit(run(context.Background(), os.Args[1:])) }

func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("xrscreen", flag.ContinueOnError)
	frames := fs.Int("frames", 300, "how many frames to paint")
	fps := fs.Float64("fps", 30, "how fast to paint them")
	width := fs.Int("width", 0, "frame width in pixels, 0 for the display's own")
	height := fs.Int("height", 0, "frame height in pixels, 0 for the display's own")
	depth := fs.Int("queue", 0, "how many frame slots, 0 for the default")
	hold := fs.Duration("hold", 10*time.Second, "how long to leave the last frame up")
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for a display that will take a presentation")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var out strings.Builder
	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		out.WriteString(line + "\n")
	}
	defer func() { save(out.String(), say) }()

	// ⛔ WallAvailable, NOT Available. Available asks whether the CAPTURE host
	// is there, and nothing here needs it: a screen is a Presentation, which
	// takes no projection, no consent and no foreground service. Checking the
	// wrong one would refuse to run on a device where this works perfectly.
	if !android.WallAvailable() {
		say("FAIL no wall host: this must run inside the APK")
		return 1
	}
	target, err := awaitGlasses(ctx, *wait, say)
	if err != nil {
		say("FAIL %v", err)
		return 1
	}
	say("TARGET %s", target)

	scr, err := android.ShowOn(ctx, target, android.ScreenOptions{
		Width: *width, Height: *height, QueueDepth: *depth,
	})
	if err != nil {
		say("FAIL ShowOn: %v", err)
		return 1
	}
	defer func() {
		if err := scr.Close(); err != nil {
			say("⚠ closing the screen: %v", err)
		}
	}()
	w, h := scr.Size()
	say("SCREEN %s", scr)

	start := time.Now()
	tick := time.NewTicker(time.Duration(float64(time.Second) / *fps))
	defer tick.Stop()
	for i := 0; i < *frames; i++ {
		c, err := scr.Next(ctx)
		if err != nil {
			say("FAIL Next at frame %d: %v", i, err)
			return 1
		}
		paint(c, i, *frames)
		if err := scr.Present(ctx, c); err != nil {
			say("FAIL Present at frame %d: %v", i, err)
			return 1
		}
		select {
		case <-ctx.Done():
			say("FAIL cancelled after %d frames", i)
			return 1
		case <-tick.C:
		}
	}
	elapsed := time.Since(start)
	st := scr.Stats()
	say("PAINTED %d frames of %dx%d in %s, %.1f fps", st.Presented, w, h,
		elapsed.Round(time.Millisecond), float64(st.Presented)/elapsed.Seconds())
	say("STATS %s", st)

	// ⛔ THE ACKNOWLEDGEMENTS ARE THE ONLY THING THIS PROCESS CAN PROVE, and a
	// screen that silently did nothing would show here as presented frames that
	// were never acknowledged. Reporting the number is not the same claim as
	// reporting a picture, and saying which is which is the point.
	if st.Acknowledged == 0 {
		say("RESULT the host took NONE of the %d frames: the mechanism is not working",
			st.Presented)
		return 1
	}
	say("RESULT the host copied %d of %d frames onto display %d and handed each to the view",
		st.Acknowledged, st.Presented, scr.DisplayID())
	say("⚠ THAT IS NOT PROOF OF LIGHT. Look through the glasses: a correct frame is " +
		"GREEN top-left, BLUE bottom-right, RED elsewhere, with a WHITE bar sweeping " +
		"left to right. Anything else -- black, a frozen frame, torn rows -- is a failure " +
		"this process cannot see.")

	if *hold > 0 {
		say("HOLDING the last frame for %s", *hold)
		select {
		case <-ctx.Done():
		case <-time.After(*hold):
		}
	}
	return 0
}

// awaitGlasses waits for a display to present on, reporting what it saw.
//
// ⛔ IT WAITS RATHER THAN ASKING ONCE, because the phone has ONE USB-C port and
// the glasses want all of it: this process is started over the cable that then
// has to be unplugged to attach them. A command that read the display list once
// at startup could only ever report the phone's own panel.
//
// ⚠ IT TAKES THE FIRST, AND SAYS SO on the transcript's TARGET line. A phone
// with two external displays is not a case this measures, and picking silently
// would leave no record of a choice nobody made.
func awaitGlasses(ctx context.Context, budget time.Duration, say func(string, ...any)) (android.Display, error) {
	deadline := time.Now().Add(budget)
	seen := 0
	for {
		ds, err := android.Screens(ctx)
		if err != nil {
			return android.Display{}, fmt.Errorf("Screens: %w", err)
		}
		if len(ds) > 0 {
			return ds[0], nil
		}
		if seen == 0 {
			say("WAITING up to %s for a display that will take a presentation; "+
				"there is none now", budget)
		}
		seen++
		if !time.Now().Before(deadline) {
			// ⚠ A TIMEOUT IS A MEASUREMENT TOO. It says Android never offered a
			// display an ordinary application may present on, which is a
			// different claim from the platform having refused one.
			return android.Display{}, fmt.Errorf("no display took a presentation in %s; "+
				"attach the glasses to the USB-C port", budget)
		}
		select {
		case <-ctx.Done():
			return android.Display{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// paint draws one frame: the sentinel quadrants with a bar sweeping across.
//
// It goes through image/draw rather than writing bytes, because that is how an
// application will use this -- and if the stride were wrong, drawing this way is
// what shears visibly rather than failing.
func paint(c android.Canvas, i, n int) {
	img := c.RGBA()
	w, h := c.Width, c.Height
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{R: 0xff, A: 0xff}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, 0, w/2, h/2),
		&image.Uniform{color.RGBA{G: 0xff, A: 0xff}}, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(w/2, h/2, w, h),
		&image.Uniform{color.RGBA{B: 0xff, A: 0xff}}, image.Point{}, draw.Src)

	// The bar is what distinguishes a live screen from one frozen on its first
	// frame, which is the failure a still image cannot show.
	bar := w / 32
	if bar < 1 {
		bar = 1
	}
	x := 0
	if n > 1 {
		x = (w - bar) * i / (n - 1)
	}
	draw.Draw(img, image.Rect(x, 0, x+bar, h),
		&image.Uniform{color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}}, image.Point{}, draw.Src)
}

// save writes the transcript where it can be read once the cable comes back,
// and says the path out loud: a file nobody can name is a file nobody will
// find, and the person who needs it was not watching when it was written.
func save(text string, say func(string, ...any)) {
	path, err := android.SaveTranscript("screen.txt", text)
	if err != nil {
		say("⚠ the transcript could not be saved: %v", err)
		return
	}
	fmt.Printf("ARTIFACT %s\n", path)
}
