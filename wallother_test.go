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

// The off-Android stub. A stub nobody tests is a stub that panics the day
// somebody links it, and a compositor that drives one of these packages per
// platform links this one on every workstation.

func TestWallAvailableOffAndroid(t *testing.T) {
	t.Parallel()
	if WallAvailable() {
		t.Fatal("WallAvailable reported a host on a platform that has none")
	}
}

func TestOpenOwnedDisplayOffAndroid(t *testing.T) {
	t.Parallel()
	f, err := openOwnedDisplay(context.Background(), goodSpec())
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("openOwnedDisplay = %v, want ErrUnsupported", err)
	}
	if f.stream != nil || f.id != 0 || f.close != nil {
		t.Fatalf("openOwnedDisplay returned a feed alongside its refusal: %+v", f)
	}
}

// The limit is portable, so it must refuse identically on a platform with no
// host at all — before ErrUnsupported, because the check comes first.
func TestWallLimitIsEnforcedOffAndroidToo(t *testing.T) {
	t.Parallel()
	w, err := NewWall(1)
	if err != nil {
		t.Fatalf("NewWall: %v", err)
	}
	if _, err := w.Open(context.Background(), goodSpec()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("the first Open = %v, want ErrUnsupported", err)
	}
	// With the transport stubbed out, the limit alone must still bite.
	w.opener = stubOpener(1)
	if _, err := w.Open(context.Background(), goodSpec()); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := w.Open(context.Background(), goodSpec()); !errors.Is(err, ErrTooManyDisplays) {
		t.Fatalf("Open past the limit = %v, want ErrTooManyDisplays", err)
	}
	if _, err := NewWall(MaxDisplays + 1); !errors.Is(err, ErrTooManyDisplays) {
		t.Fatalf("NewWall past the ceiling = %v, want ErrTooManyDisplays", err)
	}
}
