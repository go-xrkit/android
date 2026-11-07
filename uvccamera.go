// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import "time"

// UVCCameraOptions says how to open a headset's UVC camera.
//
// The zero value is usable: no wait, and whatever format [PickFormat] chooses.
//
// ⚠ "UVC" is in the name because [Camera] is already the camera2 API's, and
// these are not the same device seen two ways: this phone offers NO external
// camera2 device at all, so the headset's camera exists only through usbfs.
type UVCCameraOptions struct {
	// Wait is how long to wait for a UVC camera to appear before giving up.
	//
	// ⚠ ZERO DOES NOT WAIT, which is right for a desk starting up and wrong for
	// a probe somebody is about to plug a headset into. The glasses are a
	// cable: a caller that wants the camera "when it arrives" says so.
	Wait time.Duration
}

// streamingInterface picks the interface that carries video.
//
// ⛔ BY CLASS AS WELL AS BY ENDPOINT, and the second loop is not redundancy.
// Alternate 0 of a streaming interface has no endpoint at all — that is what
// zero-bandwidth means — so a camera whose only listed alternate is the
// zero-bandwidth one would read as having no streaming interface and the whole
// open would fail with "no UVC streaming interface".
//
// ⭐ Extracted from cmd/xrhead, where it was a pure function nothing could
// reach: the desk needs this same choice to follow a head on Android, and two
// implementations of "which interface carries video" is how one of them drifts.
func streamingInterface(ifaces []USBInterface) (int, bool) {
	for _, i := range ifaces {
		if i.VideoStreaming() && len(i.Endpoints) > 0 {
			return i.Number, true
		}
	}
	for _, i := range ifaces {
		if i.VideoStreaming() {
			return i.Number, true
		}
	}
	return 0, false
}
