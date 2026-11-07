// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"context"
	"errors"
	"testing"
)

// The off-phone camera stubs.
//
// ⛔⛔ A COMPOSITOR LINKS THIS PACKAGE ON EVERY WORKSTATION, and go-xrkit/desk
// is the proof: it builds for darwin, linux, windows and android from the same
// source and asks for a head on the one platform that has a camera. Asking the
// others must REFUSE rather than panic, and must refuse with a sentinel a
// caller can test for — desk turns ErrUnsupported into "this platform has no
// way to open one", which is a sentence a person can act on.
func TestTheCameraOffTheDeviceRefusesRatherThanPanics(t *testing.T) {
	t.Parallel()

	c, err := OpenUVCCamera(context.Background(), UVCCameraOptions{}, nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("OpenUVCCamera = %v, want ErrUnsupported", err)
	}
	// ⛔ AND NO CAMERA ALONGSIDE THE REFUSAL. A caller that checks the error
	// first is the careful one; a caller that checks the value first must not be
	// handed something that looks open.
	if c != nil {
		t.Fatalf("a camera came back alongside the refusal: %+v", c)
	}
}

// ⚠ AND THE ZERO VALUE ANSWERS AS A CLOSED CAMERA WOULD, because a deferred
// Close does not know which platform it is on. Every method is reachable on a
// value a caller may hold without having opened anything.
func TestAStubCameraAnswersAsAClosedOneWould(t *testing.T) {
	t.Parallel()

	var c UVCCamera
	if _, err := c.Frame(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Frame = %v, want ErrUnsupported", err)
	}
	// ⛔ nil, NOT AN EMPTY STREAM. One with no frames in it reads as a camera
	// that delivered nothing, which is a different statement from "there is no
	// camera on this platform".
	if s := c.Stream(); s != nil {
		t.Errorf("Stream = %v, want nil where there is no camera", s)
	}
	// ⛔ AND NOTHING WAS TAKEN FROM uvcvideo, so the replug warning must stay
	// silent: a caller printing "unplug and replug the headset" on a Mac would
	// be telling somebody to fix a phone they are not holding.
	if d := c.Detached(); d != nil {
		t.Errorf("Detached = %v, want nothing taken", d)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("the second Close = %v, want nil", err)
	}
}
