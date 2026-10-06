// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrcam says whether this phone can reach the glasses' camera, and by
// which route.
//
// # Why this is a measurement and not a design decision
//
// An XR ribbon has to follow a head, and a VITURE Beast WILL NOT SAY where it
// is pointing: it holds its own 3DOF tracking and anchors the picture it is
// given with it, but publishes no orientation at all — measured three ways on
// 2026-09-07 and none of them produced a number. Its camera, however, is an
// ordinary UVC device, and a turn of the head is plainly visible in it;
// go-xrkit/xrkit/headflow already recovers the yaw from that, at 1.14% residual
// over a there-and-back sweep.
//
// On macOS that camera is an AVFoundation device and the matter ends there. On
// Android there are exactly two routes and NEITHER IS GUARANTEED:
//
//   - camera2, if this phone's HAL enumerates external USB cameras. Android
//     defines LENS_FACING_EXTERNAL for precisely this and leaves supporting it
//     to the vendor, so it is a per-device question with no documentation that
//     settles it;
//   - the USB host API, claiming the UVC interface — which only works if the
//     streaming endpoints are BULK, because Android's Java USB API cannot
//     submit an isochronous transfer at all.
//
// ⛔ WRITING THE TRANSPORT FIRST AND FINDING OUT AFTERWARDS IS THE EXPENSIVE
// ORDER. This asks, costs nobody a permission dialog, and says which of the two
// is open — including when the answer is neither.
//
//	APP=./cmd/xrcam host/build.sh && adb install -r host/out/xrhost.apk
//	adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity \
//	    --ez capture false --es args '-wait 20m'
//	# unplug the cable, attach the glasses, wait, plug the cable back
//	adb shell run-as org.goxrkit.androidhost cat files/camera.txt
//
// ⚠ IT TAKES A WITNESS FIRST, like cmd/xrglasses: the census BEFORE the glasses
// are attached is recorded, so "a camera appeared" is a comparison rather than
// a guess. A phone has cameras of its own and USB devices of its own, and a
// census without a baseline would credit the headset with the phone's own
// hardware.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-xrkit/android"
)

func main() { os.Exit(run(context.Background(), os.Args[1:])) }

func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("xrcam", flag.ContinueOnError)
	wait := fs.Duration("wait", 5*time.Minute, "how long to wait for the glasses to be attached")
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

	// The wall host, not the capture host: nothing here needs a projection, a
	// consent dialog or a foreground service.
	if !android.WallAvailable() {
		say("FAIL no wall host: this must run inside the APK")
		return 1
	}

	before, err := census(ctx)
	if err != nil {
		say("FAIL the census before: %v", err)
		return 1
	}
	say("BEFORE, with nothing attached:")
	report(say, before)

	say("")
	say("WAITING up to %s for the glasses", *wait)
	after, ok := awaitGlasses(ctx, *wait, before, say)
	if !ok {
		return 1
	}
	say("")
	say("AFTER, with the glasses attached:")
	report(say, after)

	// ⛔ THE DIFFERENCE, NOT THE TOTAL. A phone's own cameras are in both
	// censuses, and crediting the headset with them is the mistake this whole
	// before-and-after exists to prevent.
	say("")
	newCams := newCameras(before, after)
	newDevs := newDevices(before, after)
	say("APPEARED %d camera(s) and %d USB device(s) that were not there before",
		len(newCams), len(newDevs))
	for _, c := range newCams {
		say("  %s", c)
	}
	for _, d := range newDevs {
		say("  %s", d)
	}

	// ⭐ WHAT IS STILL OPEN, said whatever the camera verdict is. A headset
	// whose camera is isochronous is not a headset with nothing to read, and a
	// transcript that stopped at "no" would send the next person away from
	// interfaces this API can carry today.
	say("")
	say("READABLE through Android's own USB API (bulk and interrupt only):")
	any := false
	for _, d := range newDevs {
		for _, i := range d.Readable() {
			say("  %04x:%04x %s", d.VendorID, d.ProductID, i)
			any = true
		}
	}
	if !any {
		say("  nothing: every IN endpoint on every device that appeared is isochronous")
	}

	route := android.ChooseRoute(newCams, newDevs)
	say("")
	say("ROUTE %s", route)
	switch route {
	case android.RouteCamera2:
		say("RESULT the platform offers the headset's camera as an EXTERNAL camera2 device. " +
			"That is the route to take: Android does the UVC negotiation, the format " +
			"conversion and the buffering, and headflow only needs the frames.")
		return 0
	case android.RouteUSBBulk:
		say("RESULT no external camera2 device, but a UVC interface whose streaming " +
			"endpoints are BULK. Android's Java USB API can read those, so the route is " +
			"UsbDeviceConnection with the UVC negotiation written by hand.")
		return 0
	case android.RouteUSBIsochronous:
		say("RESULT a UVC camera is there and NEITHER Android API can read it: no " +
			"external camera2 device, and the streaming endpoints are isochronous, " +
			"which UsbDeviceConnection cannot submit at all. Reaching it means usbfs " +
			"ioctls on the descriptor that API hands out — possible from CGO-free Go, " +
			"and a great deal more work than either of the other two.")
		return 1
	}
	say("RESULT nothing appeared that could carry a camera. Either the glasses expose " +
		"no camera over this cable, or they expose it only in a mode this phone did " +
		"not enter. ⚠ A USB-C splitter is one known cause: a data-only splitter " +
		"carries no DisplayPort and may carry no camera either.")
	return 1
}

// snapshot is one census: what the phone could see at one moment.
type snapshot struct {
	cameras []android.Camera
	devices []android.USBDevice
}

func census(ctx context.Context) (snapshot, error) {
	cams, err := android.Cameras(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("Cameras: %w", err)
	}
	devs, err := android.USBDevices(ctx)
	if err != nil {
		return snapshot{}, fmt.Errorf("USBDevices: %w", err)
	}
	return snapshot{cameras: cams, devices: devs}, nil
}

func report(say func(string, ...any), s snapshot) {
	say("  %d camera(s):", len(s.cameras))
	for _, c := range s.cameras {
		say("    %s", c)
	}
	say("  %d USB device(s):", len(s.devices))
	for _, d := range s.devices {
		say("    %s", d)
	}
}

// awaitGlasses waits until a display that takes a presentation appears, then
// takes the census again.
//
// ⭐ THE DISPLAY IS THE SIGNAL, NOT THE CAMERA. Waiting for a camera to appear
// would be waiting for the answer: a run where none ever did could not tell
// "the glasses were never plugged in" from "they were, and offer nothing". The
// display says the headset is attached, and the census is then an answer about
// a headset that is definitely there.
func awaitGlasses(ctx context.Context, budget time.Duration, before snapshot,
	say func(string, ...any)) (snapshot, bool) {
	deadline := time.Now().Add(budget)
	for {
		scr, err := android.Screens(ctx)
		if err != nil {
			say("FAIL Screens while waiting: %v", err)
			return snapshot{}, false
		}
		if len(scr) > 0 {
			say("ATTACHED %s", scr[0])
			// The display arrives before the rest of the headset enumerates:
			// a census taken the same instant can miss a camera that is still
			// coming up, and would report it as absent.
			select {
			case <-ctx.Done():
				say("FAIL cancelled while settling")
				return snapshot{}, false
			case <-time.After(3 * time.Second):
			}
			after, err := census(ctx)
			if err != nil {
				say("FAIL the census after: %v", err)
				return snapshot{}, false
			}
			return after, true
		}
		if !time.Now().Before(deadline) {
			say("FAIL no display took a presentation in %s, so the glasses were never "+
				"attached and this measured nothing", budget)
			return snapshot{}, false
		}
		select {
		case <-ctx.Done():
			say("FAIL cancelled while waiting")
			return snapshot{}, false
		case <-time.After(2 * time.Second):
		}
	}
}

// newCameras returns the cameras in after that were not in before.
func newCameras(before, after snapshot) []android.Camera {
	known := make(map[string]bool, len(before.cameras))
	for _, c := range before.cameras {
		known[c.ID] = true
	}
	var out []android.Camera
	for _, c := range after.cameras {
		if !known[c.ID] {
			out = append(out, c)
		}
	}
	return out
}

// newDevices returns the USB devices in after that were not in before.
func newDevices(before, after snapshot) []android.USBDevice {
	known := make(map[string]bool, len(before.devices))
	for _, d := range before.devices {
		known[d.Name] = true
	}
	var out []android.USBDevice
	for _, d := range after.devices {
		if !known[d.Name] {
			out = append(out, d)
		}
	}
	return out
}

// save writes the transcript where it can be read once the cable comes back.
func save(text string, say func(string, ...any)) {
	path, err := android.SaveTranscript("camera.txt", text)
	if err != nil {
		say("⚠ the transcript could not be saved: %v", err)
		return
	}
	fmt.Printf("ARTIFACT %s\n", path)
}
