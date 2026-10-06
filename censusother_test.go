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

// The off-Android census stubs. The census is what decides whether a headset's
// camera can be reached at all, so a compositor asks it on every platform — and
// a stub that panicked instead of refusing would take the whole application
// down on a workstation.

func TestCamerasOffAndroid(t *testing.T) {
	t.Parallel()
	cs, err := Cameras(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Cameras = %v, want ErrUnsupported", err)
	}
	if cs != nil {
		t.Fatalf("Cameras returned %v alongside its refusal", cs)
	}
}

func TestUSBDevicesOffAndroid(t *testing.T) {
	t.Parallel()
	ds, err := USBDevices(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("USBDevices = %v, want ErrUnsupported", err)
	}
	if ds != nil {
		t.Fatalf("USBDevices returned %v alongside its refusal", ds)
	}
}
