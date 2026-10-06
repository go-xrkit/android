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

// The off-Android stub for screens. A stub nobody tests is a stub that panics
// the day somebody links it, and a compositor that drives one of these packages
// per platform links this one on every workstation.

func TestScreensOffAndroid(t *testing.T) {
	t.Parallel()
	ds, err := Screens(context.Background())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Screens = %v, want ErrUnsupported", err)
	}
	if ds != nil {
		t.Fatalf("Screens returned %v alongside its refusal", ds)
	}
}

func TestOpenScreenOffAndroid(t *testing.T) {
	t.Parallel()
	f, err := openScreen(context.Background(), glasses, ScreenOptions{
		Width: 1920, Height: 1080, QueueDepth: 3})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("openScreen = %v, want ErrUnsupported", err)
	}
	if f.buf != nil || f.present != nil || f.close != nil {
		t.Fatalf("openScreen returned a feed alongside its refusal: %+v", f)
	}
}

// ⛔ THE REFUSALS COME IN THAT ORDER EVERYWHERE. A display that cannot carry a
// presentation, and options that cannot describe a frame, are answered from the
// Display and the options alone — so a workstation gets the SAME message a
// phone does, rather than ErrUnsupported hiding the real reason.
func TestShowOnRefusesBeforeTheStubOffAndroid(t *testing.T) {
	t.Parallel()
	builtin := Display{ID: DefaultDisplayID, Width: 2152, Height: 2076}
	if _, err := ShowOn(context.Background(), builtin, ScreenOptions{}); !errors.Is(err, ErrNotPresentable) {
		t.Fatalf("ShowOn on the built-in panel = %v, want ErrNotPresentable", err)
	}
	if _, err := ShowOn(context.Background(), glasses, ScreenOptions{QueueDepth: 99}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("ShowOn with 99 slots = %v, want ErrInvalidOption", err)
	}
	if _, err := ShowOn(context.Background(), glasses, ScreenOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ShowOn with good arguments = %v, want ErrUnsupported", err)
	}
}
