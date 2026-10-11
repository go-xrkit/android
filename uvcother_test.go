// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"errors"
	"testing"
)

// The off-Android UVC stubs. A compositor links this package on every
// workstation, and asking a camera to negotiate where there is none must refuse
// rather than panic.

func TestNegotiateOffAndroid(t *testing.T) {
	t.Parallel()
	var h USBHandle
	n, err := h.Negotiate(1, UVCFormatDesc{}, UVCFrameSize{})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Negotiate = %v, want ErrUnsupported", err)
	}
	if n != (UVCNegotiation{}) {
		t.Fatalf("Negotiate returned %v alongside its refusal", n)
	}
}

func TestControlOffAndroid(t *testing.T) {
	t.Parallel()
	var h USBHandle
	n, err := h.Control(ControlIn(), 0x81, 0x0100, 1, make([]byte, 26))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Control = %v, want ErrUnsupported", err)
	}
	if n != 0 {
		t.Fatalf("Control moved %d bytes on a platform with no device", n)
	}
}
