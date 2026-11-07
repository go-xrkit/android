// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrhead tracks a head ON THE PHONE, live, from the headset's camera.
//
// # What it answers that the replay could not
//
// cmd/xrusb writes frames out and go-xrkit/xrkit/cmd/headflowreplay reads them
// back, which proved the chain and measured a worn sweep at 197.9° of travel.
// What it could not say is whether the PHONE keeps up: the tracker ran on a
// workstation, against files, with no clock. Here it runs in the same process as
// the stream, on the processor that would have to do it in a real application,
// and it reports what each stage costs.
//
// ⛔⛔ AND THE DEPENDENCY THIS NEEDED COST ONE LINE, against an argument this
// repository made four times. "go-xrkit/xrkit pulls in HCL and eight indirect
// modules, and a USB layer has no business acquiring a configuration language"
// — measured, importing headflow adds ONE line to go.mod and TWO to go.sum,
// because Go prunes a dependency's requirements for packages you do not import
// and headflow imports only the standard library. The file interface that
// argument justified is still worth having — offline replay, and pictures a
// person can look at — but it was justified by a reason that was not true, and
// the reason is corrected rather than quietly dropped.
//
//	go run ./tools/xrrun -app ./cmd/xrhead -args '-wait 20s -for 10s' \
//	    -perms android.permission.CAMERA
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-xrkit/android"
	"github.com/go-xrkit/xrkit/headflow"
)

func main() { os.Exit(run(context.Background(), os.Args[1:])) }

func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("xrhead", flag.ContinueOnError)
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for a UVC camera")
	dur := fs.Duration("for", 10*time.Second, "how long to track")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var out strings.Builder
	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		out.WriteString(line + "\n")
	}
	defer func() {
		if path, err := android.SaveTranscript("head.txt", out.String()); err != nil {
			say("⚠ the transcript could not be saved: %v", err)
		} else {
			fmt.Printf("ARTIFACT %s\n", path)
		}
	}()

	// ⭐ THE NINE-STEP OPEN LIVES IN THE PACKAGE NOW. It was written here, and
	// go-xrkit/desk needs the identical sequence to follow a head on Android —
	// two copies of an order that is only correct in one order is how one of
	// them drifts. The stage lines below are unchanged: they are this probe's
	// measurement protocol, quoted in the README, so android.OpenCamera prints
	// them through the same `say`.
	cam, err := android.OpenUVCCamera(ctx, android.UVCCameraOptions{Wait: *wait}, say)
	if err != nil {
		say("FAIL open camera: %v", err)
		return 1
	}
	defer func() {
		if err := cam.Close(); err != nil {
			say("⚠ %v", err)
		}
		if d := cam.Detached(); len(d) > 0 {
			// ⛔ PRINTED WHETHER THIS SUCCEEDED OR NOT. Claiming the streaming
			// interface takes it from uvcvideo and the reconnect does not
			// restore the binding on this phone, so the camera is left with no
			// driver until the headset is replugged. A run that failed is
			// exactly the one nobody reads to the end of.
			say("⛔ INTERFACE %v WAS TAKEN FROM uvcvideo and not given back. UNPLUG "+
				"AND REPLUG the headset to return it to the system.", d)
		}
	}()

	return track(ctx, cam.Stream(), cam.Size, *dur, say)
}

// track feeds every frame to the tracker and reports what each stage costs.
//
// ⛔⛔ THE TRACKER'S OWN TIME IS MEASURED SEPARATELY FROM THE STREAM'S, because
// the question is which one bounds the other. The camera gives about ten frames
// a second — a ceiling of its own, measured across a fourfold change in pixel
// count — so a tracker costing a few milliseconds is free and one costing ninety
// is the new ceiling. Nothing here may be reported as "keeps up" without the
// number beside it.
func track(ctx context.Context, s *android.UVCStream, size android.UVCFrameSize,
	dur time.Duration, say func(string, ...any)) int {
	rctx, cancel := context.WithTimeout(ctx, dur)
	defer cancel()

	var tr headflow.Tracker
	var tracking time.Duration
	frames, used, refused, references := 0, 0, 0, 0
	var travel, prev float64
	var minYaw, maxYaw float64
	start := time.Now()
	var stopped error

	// ⛔⛔ DO CONSECUTIVE FRAMES DIFFER AT ALL? Without this the probe cannot
	// tell "the head did not move" from "this wiring is blind", and the first
	// two runs reported 0.000° of travel with 89 frames matched at good
	// confidence — a reading consistent with BOTH. An instrument that cannot
	// fail is not an instrument, and the stream hands back a REUSED image, which
	// is exactly the shape of a bug that would produce a confident zero.
	//
	// ⚠ It is one row of the middle band, which is the band headflow reads, and
	// it costs a few hundred comparisons a frame against a 93 ms interval.
	var lastRow []byte
	same, different := 0, 0
	for {
		img, err := s.Frame(rctx)
		if err != nil {
			stopped = err
			break
		}
		frames++
		y := size.Height / 2
		row := img.Pix[y*img.Stride : y*img.Stride+size.Width*4]
		if lastRow != nil {
			if bytesEqual(lastRow, row) {
				same++
			} else {
				different++
			}
		}
		lastRow = append(lastRow[:0], row...)
		t0 := time.Now()
		yaw, ok := tr.Feed(img)
		tracking += time.Since(t0)
		deg := yaw * 180 / 3.141592653589793
		switch {
		case ok:
			used++
			travel += abs(deg - prev)
			prev = deg
			minYaw, maxYaw = min(minYaw, deg), max(maxYaw, deg)
		case tr.Unusable() == 0:
			// ⛔ A REFERENCE IS NOT A REFUSAL: it moves nothing by definition,
			// there being nothing yet to compare it against, and headflow
			// distinguishes the two through Unusable.
			references++
		default:
			refused++
		}
	}
	elapsed := time.Since(start)
	st := s.Stats()

	say("")
	say("TRACKED %d frame(s) in %s — %.3g fps", frames, elapsed.Round(time.Millisecond),
		float64(frames)/elapsed.Seconds())
	say("    %d used, %d reference(s), %d refused for want of confidence",
		used, references, refused)
	say("    %s", st)
	if frames > 0 {
		say("    the tracker itself: %s in all, %s a frame",
			tracking.Round(time.Millisecond),
			(tracking / time.Duration(frames)).Round(10*time.Microsecond))
	}
	if st.Frames > 0 && st.Decoding > 0 {
		say("    the decode: %s a frame", (st.Decoding /
			time.Duration(st.Frames)).Round(10*time.Microsecond))
	}
	// ⛔⛔ WHAT THIS CONTROL DOES AND DOES NOT SAY. It was added because "0° of
	// travel" is consistent with a still head AND with wiring that hands back
	// one picture — and it answers the second question only. IDENTICAL frames
	// mean the stream is not delivering: a camera on a real room produces sensor
	// noise and is never byte-identical. DIFFERENT frames mean nothing about
	// movement, because that same noise changes every pixel while the column
	// profile still matches at a shift of zero. Reading them the other way round
	// had this probe accusing its own plumbing on three consecutive runs.
	say("    consecutive frames: %d differed, %d identical", different, same)
	if stopped != nil && !errors.Is(stopped, context.DeadlineExceeded) {
		say("    STOPPED BY: %v", stopped)
	}

	switch {
	case frames == 0:
		say("ANSWER NO FRAMES. Nothing arrived, so nothing was tracked: %v", stopped)
		return 1
	case used == 0:
		say("ANSWER NOTHING COULD BE MATCHED in %d frame(s). That is the scene rather "+
			"than the phone: below a confidence of %.1f the picture has nothing in it "+
			"to match, which is what a blank wall or a dark room looks like.",
			frames, headflow.MinConfidence)
		return 1
	case travel < 1 && different == 0:
		// ⛔⛔ IDENTICAL FRAMES ARE THE PLUMBING CONCERN, and this is the branch
		// that earns its keep. The stream hands back a REUSED image; if its
		// pixels never change between calls, the camera is sending duplicates
		// or nothing is being written into it, and a tracker reading one picture
		// would report a confident zero forever.
		say("⛔ ANSWER EVERY FRAME WAS IDENTICAL. %d frame(s) matched at confidence "+
			"%.3f and no two consecutive frames differed by a single byte in the "+
			"band the tracker reads. A camera pointed at a real room produces "+
			"sensor noise, so this is the stream handing back one picture rather "+
			"than a still scene.", used, tr.Confidence())
		return 1
	case travel < 1:
		// ⛔⛔ AND THIS BRANCH USED TO ACCUSE THE PLUMBING, WRONGLY. It said "the
		// pixels changed and the yaw did not, so a still head cannot produce
		// that" — but SENSOR NOISE changes every pixel of every frame without
		// the view moving at all, and the column profile still matches at a
		// shift of zero with a confidence of 0.99. Three runs in a row were
		// reported as a fault in this wiring when the wiring was fine and the
		// head was simply still. "The pixels changed" is not "the camera moved",
		// and an instrument that accuses the wrong thing is worse than one that
		// says nothing.
		say("ANSWER THE VIEW DID NOT PAN. %d frame(s) matched at confidence %.3f and "+
			"the view travelled %.3f°; %d of %d consecutive frames differed, which "+
			"is sensor noise rather than movement — a camera on a real room is "+
			"never byte-identical. That is the useful negative: a tracker that "+
			"invented movement would show it here. Turn your head and run it again.",
			used, tr.Confidence(), travel, different, different+same)
		return 0
	}
	// ⭐ THE BUDGET, WHICH IS THE POINT OF RUNNING THIS ON THE PHONE. The frame
	// interval is what everything has to fit inside.
	interval := elapsed / time.Duration(max(frames, 1))
	perFrame := tracking / time.Duration(max(frames, 1))
	say("ANSWER %.1f° of travel, %.1f° to %.1f°, over %d matched frame(s)",
		travel, minYaw, maxYaw, used)
	say("RESULT the phone tracks its own head. The tracker takes %s of a %s frame "+
		"interval — %.1f%% — so what bounds this is the camera's ~10 frames a "+
		"second and not the arithmetic.", perFrame.Round(10*time.Microsecond),
		interval.Round(time.Millisecond),
		float64(perFrame)/float64(interval)*100)
	return 0
}

// bytesEqual is bytes.Equal, written out so this command's only imports are the
// two packages it is wiring together plus the standard essentials.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// streamingInterface is the UVC interface that carries frames, by number.
func streamingInterface(ifaces []android.USBInterface) (int, bool) {
	for _, i := range ifaces {
		if i.VideoStreaming() && len(i.Endpoints) > 0 {
			return i.Number, true
		}
	}
	// Alternate 0 of a streaming interface has no endpoint at all, so an
	// interface is looked for by its CLASS as well, or a camera whose only
	// listed alternate is the zero-bandwidth one would read as absent.
	for _, i := range ifaces {
		if i.VideoStreaming() {
			return i.Number, true
		}
	}
	return 0, false
}

// awaitCamera waits for a UVC device to appear, taking a census first.
//
// ⛔ THE CENSUS COMES FIRST AND NEEDS NO PERMISSION. Listing devices is not
// opening one, and printing what was attached BEFORE the wait is what turns "no
// camera appeared" into "here is what was there instead".
func awaitCamera(ctx context.Context, budget time.Duration,
	say func(string, ...any)) (android.USBDevice, bool) {
	deadline := time.Now().Add(budget)
	first := true
	for {
		devs, err := android.USBDevices(ctx)
		if err != nil {
			say("FAIL USBDevices: %v", err)
			return android.USBDevice{}, false
		}
		if first {
			say("BEFORE %d USB device(s) attached:", len(devs))
			for _, d := range devs {
				say("  %s %04x:%04x %q", d.Name, d.VendorID, d.ProductID, d.Product)
			}
			first = false
		}
		for _, d := range devs {
			if len(d.VideoStreaming()) > 0 {
				return d, true
			}
		}
		if !time.Now().Before(deadline) {
			say("FAIL no UVC camera appeared in %s", budget)
			return android.USBDevice{}, false
		}
		select {
		case <-ctx.Done():
			return android.USBDevice{}, false
		case <-time.After(500 * time.Millisecond):
		}
	}
}
