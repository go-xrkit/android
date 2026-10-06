// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrglasses waits for XR glasses to be attached to the phone and says
// exactly what Android makes of them.
//
// It exists because the phone has ONE USB-C port, and the glasses want all of
// it: attaching them takes the cable that adb was using. So this cannot be a
// question asked interactively -- it has to be a question ASKED IN ADVANCE and
// answered while nobody is watching.
//
//	APP=./cmd/xrglasses host/build.sh && adb install -r host/out/xrhost.apk
//	adb shell am start -n org.goxrkit.androidhost/org.goxrkit.android.XrDemoActivity --ez capture false
//	# unplug the cable, attach the glasses, wait, plug the cable back
//	adb logcat -d -s xrcapture
//	adb shell cat /storage/emulated/0/Android/data/org.goxrkit.androidhost/files/glasses.txt
//
// ⛔ IT TAKES A WITNESS FIRST. The displays present BEFORE the glasses are
// attached are recorded, so "a display appeared" is a comparison rather than a
// guess -- a Fold has two built-in panels, inner and cover, and the cover one
// comes and goes as the phone is opened and closed. Counting displays without a
// baseline would report the hinge as a pair of glasses.
//
// ⚠ AND IT WRITES TO A FILE AS WELL AS TO logcat. The log is a ring buffer on
// the device: an answer that arrives while the cable is elsewhere can be gone
// by the time anybody can read it. The file is what survives.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-xrkit/android"
)

func main() { os.Exit(run(context.Background())) }

// patience is how long to wait for the glasses. Long, because the cable has to
// be moved by hand and the person doing it is wearing the thing they are
// plugging in.
const patience = 5 * time.Minute

func run(ctx context.Context) int {
	var out strings.Builder
	say := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Println(line)
		out.WriteString(line + "\n")
	}
	defer func() { save(out.String(), say) }()

	if !android.Available() {
		say("FAIL no host: this must run inside the APK")
		return 1
	}

	before, err := android.Displays(ctx)
	if err != nil {
		say("FAIL Displays: %v", err)
		return 1
	}
	known := map[int]bool{}
	say("BEFORE %d display(s) with no glasses attached:", len(before))
	for _, d := range before {
		known[d.ID] = true
		say("  %s", describe(d))
	}

	say("WAITING up to %s for a display that was not there", patience)
	deadline := time.Now().Add(patience)
	for time.Now().Before(deadline) {
		now, err := android.Displays(ctx)
		if err != nil {
			say("FAIL Displays while waiting: %v", err)
			return 1
		}
		for _, d := range now {
			if known[d.ID] {
				continue
			}
			// ⭐ THE NAME IS THE ANSWER. A display attached over USB-C DP Alt
			// Mode takes its name from the sink's EDID, which is how a pair of
			// glasses announces its model -- so this line says whether Android
			// sees the Beast as the Beast, or as an anonymous panel.
			say("APPEARED %s", describe(d))
			say("RESULT the glasses ARE an Android display: id %d, %dx%d, %.1f Hz, "+
				"presentation %t", d.ID, d.Width, d.Height, d.RefreshRate, d.Presentation())
			if !d.Presentation() {
				say("⚠ NOT presentation-capable: content cannot be shown there " +
					"with a Presentation, which is the only route an ordinary " +
					"application has")
				return 1
			}
			return 0
		}
		select {
		case <-ctx.Done():
			say("FAIL cancelled while waiting")
			return 1
		case <-time.After(2 * time.Second):
		}
	}
	// ⚠ A TIMEOUT IS A MEASUREMENT TOO, and it is not the same claim as a
	// refusal: it says Android never saw a new display, which on a phone whose
	// only port was taken by the glasses is the thing worth knowing.
	say("RESULT no new display in %s -- Android never saw the glasses", patience)
	return 1
}

func describe(d android.Display) string {
	return fmt.Sprintf("id %d %q %dx%d @%ddpi %.1fHz flags %#x presentation %t default %t",
		d.ID, d.Name, d.Width, d.Height, d.DensityDPI, d.RefreshRate,
		d.Flags, d.Presentation(), d.Default())
}

// save writes the transcript where it can be read after the cable comes back.
//
// ⛔ THE PATH IS SAID OUT LOUD, because a file nobody can name is a file nobody
// will find -- and the whole point of writing it is that the person who needs
// it was not watching when it was written.
// ⛔ THE APP'S OWN files DIRECTORY, NOT ITS EXTERNAL ONE. A transcript in
// /storage/emulated/0/Android/data/<pkg>/ is hidden from the shell user on a
// modern Android and `adb shell run-as` cannot traverse the FUSE layer that
// mediates it: a whole run was measured, written there, and unreadable. The
// private directory -- which the host hands over as HOME -- is what run-as can
// read, and this is a transcript rather than a capture, so android.ArtifactDir's
// rule about never writing pixels inside a work tree does not govern it.
func save(text string, say func(string, ...any)) {
	dir := os.Getenv("HOME")
	if dir == "" {
		var err error
		if dir, err = android.ArtifactDir(); err != nil {
			say("⚠ no place to write the transcript: %v", err)
			return
		}
	}
	path := filepath.Join(dir, "glasses.txt")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		say("⚠ the transcript could not be saved to %s: %v", path, err)
		return
	}
	fmt.Printf("ARTIFACT %s\n", path)
}
