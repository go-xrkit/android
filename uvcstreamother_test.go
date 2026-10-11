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

// The off-Android stream stubs. A compositor links this package on every
// workstation, and asking a camera for frames where there is no camera must
// refuse rather than panic.

func TestStreamOffAndroid(t *testing.T) {
	t.Parallel()
	var h USBHandle
	s, err := h.Stream(UVCNegotiation{}, USBEndpoint{Address: 0x81, Attributes: 1,
		MaxPacketSize: 0x1400}, 640, 480)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Stream = %v, want ErrUnsupported", err)
	}
	if s != nil {
		t.Fatal("Stream returned a stream alongside its refusal")
	}
}

// ⛔ THE PORTABLE CHECK HAPPENS EVERYWHERE, AND THAT IS THE POINT OF HAVING IT
// IN uvcframe.go. A frame size this package cannot track is refused with the
// SAME error on a workstation as on the phone, so a caller debugging a size
// does not have to be holding the device to find out.
func TestStreamOffAndroidStillChecksTheFrameSize(t *testing.T) {
	t.Parallel()
	var h USBHandle
	ep := USBEndpoint{Address: 0x81, Attributes: 1, MaxPacketSize: 0x1400}
	if _, err := h.Stream(UVCNegotiation{}, ep, 16, 16); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("Stream on a 16x16 frame = %v, want ErrInvalidOption — not "+
			"ErrUnsupported, which would hide the real mistake", err)
	}
}

func TestAStubStreamAnswersAsAStoppedOneWould(t *testing.T) {
	t.Parallel()
	neg := UVCNegotiation{Format: 1, Frame: 2, Interval: 400000}
	s := &UVCStream{neg: neg}
	if _, err := s.Frame(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Frame = %v, want ErrUnsupported", err)
	}
	if got := s.Stats(); got != (UVCStreamStats{}) {
		t.Errorf("Stats = %v, want nothing counted where nothing ran", got)
	}
	if got := s.Negotiation(); got != neg {
		t.Errorf("Negotiation = %+v, want what it was built with", got)
	}
	// Close is idempotent here too, because a caller's deferred Close does not
	// know which platform it is on.
	if err := s.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("the second Close = %v, want nil", err)
	}
}
