// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"path/filepath"
	"testing"
)

// TestADEVICESUPPLIEDNAMECANNOTLEAVETHEDIRECTORY.
//
// ⛔⛔ THIS TOOL SHIPPED WITH A PATH TRAVERSAL AND AN AUDIT OF THE SAME CHANGE
// FOUND IT. The names are read from the phone with `adb shell ls` and were
// joined to a path after checking only a prefix and a suffix. Measured, not
// reasoned about:
//
//	"frame-/../../../tmp/x.png"                 -> …/Application Support/tmp/x.png
//	"frame-/../../../../../../etc/cron.d/x.png" -> /Users/etc/cron.d/x.png
//
// Both pass "begins with frame-, ends with .png", because `frame-` is a path
// element and the `..` that follow are their own. A real filesystem cannot
// produce a basename containing a separator — but these cross a WIRELESS adb
// link from a device, and the write happens with the person's own privileges on
// their own workstation. A tool that pulls files off a device must treat their
// names as input.
func TestADeviceSuppliedNameCannotLeaveTheDirectory(t *testing.T) {
	t.Parallel()
	dir := "/Users/me/Library/Application Support/go-xrkit-android/frames"

	t.Run("the names that escaped", func(t *testing.T) {
		for _, name := range []string{
			"frame-/../../../tmp/x.png",
			"frame-/../../../../../../etc/cron.d/x.png",
			"frame-../../x.png",
			"frame-/x.png",
			"../frame-001.png",
			"sub/frame-001.png",
		} {
			if safeFrameName(name) {
				t.Errorf("safeFrameName(%q) accepted a name that is a path", name)
			}
		}
	})

	t.Run("and the ones a real capture produces", func(t *testing.T) {
		for _, name := range []string{
			"frame-001.png", "frame-030.png", "frame-999.png",
		} {
			if !safeFrameName(name) {
				t.Errorf("safeFrameName(%q) refused a name the probe writes", name)
			}
		}
	})

	t.Run("and the ones that are simply not frames", func(t *testing.T) {
		for _, name := range []string{
			"usb.txt", "frame-001.jpg", "notframe-001.png", "frame-", ".png", "", "..", ".",
		} {
			if safeFrameName(name) {
				t.Errorf("safeFrameName(%q) accepted something that is not a frame", name)
			}
		}
	})

	// ⚠ THE SECOND CHECK, WHICH DOES NOT TRUST THE FIRST. One is a rule about
	// strings and the other is a question about the filesystem, and it is the
	// second that would catch a case neither of us thought of.
	t.Run("the joined path is checked as well", func(t *testing.T) {
		for _, c := range []struct {
			path string
			want bool
		}{
			{filepath.Join(dir, "frame-001.png"), true},
			{dir, false},
			{filepath.Dir(dir), false},
			{"/etc/cron.d/x.png", false},
			{filepath.Join(dir, "deeper", "frame-001.png"), true},
		} {
			if got := insideDir(dir, c.path); got != c.want {
				t.Errorf("insideDir(%q) = %v, want %v", c.path, got, c.want)
			}
		}
	})

	// A relative directory makes filepath.Rel fail against an absolute path,
	// which must read as "not inside" rather than as "inside".
	t.Run("a comparison that cannot be made is not a yes", func(t *testing.T) {
		if insideDir("frames", "/etc/x.png") {
			t.Error("insideDir said yes when the two paths cannot be compared")
		}
	})
}
