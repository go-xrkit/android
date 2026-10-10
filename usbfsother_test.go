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

// The off-Android USB stubs. A compositor that drives one of these packages per
// platform links this one on every workstation, and a stub that panicked
// instead of refusing would take the application down there.

func TestOpenUSBOffAndroid(t *testing.T) {
	t.Parallel()
	// ⛔ THE PORTABLE REFUSAL COMES FIRST, HERE TOO. A device with no name
	// cannot be opened anywhere, and answering ErrUnsupported instead would
	// hide a caller's mistake behind the platform.
	if _, err := OpenUSB(context.Background(), USBDevice{}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("OpenUSB with an unnamed device = %v, want ErrInvalidOption", err)
	}
	h, err := OpenUSB(context.Background(), USBDevice{Name: "/dev/bus/usb/001/003"})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("OpenUSB = %v, want ErrUnsupported", err)
	}
	if h != nil {
		t.Fatalf("OpenUSB returned a handle alongside its refusal")
	}
}

func TestAUSBHandleOffAndroidRefusesEverything(t *testing.T) {
	t.Parallel()
	var h USBHandle
	if h.Name() != "" {
		t.Fatalf("Name = %q on a zero handle", h.Name())
	}
	d, err := h.Descriptors()
	if !errors.Is(err, ErrUnsupported) || d != nil {
		t.Fatalf("Descriptors = %v, %v", d, err)
	}
	if err := h.ClaimInterface(1); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ClaimInterface = %v", err)
	}
	if err := h.SetAltSetting(1, 6); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SetAltSetting = %v", err)
	}
	if err := h.SubmitISO(USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 5120}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("SubmitISO = %v", err)
	}
	// ⚠ Close reports nil, not ErrUnsupported: there is nothing to release, and
	// a caller's deferred Close must not turn a clean path into an error.
	if err := h.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
}
