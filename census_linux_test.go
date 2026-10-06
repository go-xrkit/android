// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTheCensusComesBackWholeFromTheHost(t *testing.T) {
	fw := newFakeWall(t, 0)
	cams := []Camera{
		{ID: "0", Facing: FacingBack, Width: 4000, Height: 3000},
		{ID: "2", Facing: FacingExternal, Width: 1920, Height: 1080},
	}
	devs := []USBDevice{beast(0x05)}
	fw.setCensus(cams, devs)

	gotCams, err := Cameras(screenCtx(t))
	if err != nil {
		t.Fatalf("Cameras: %v", err)
	}
	if len(gotCams) != 2 || !gotCams[1].External() {
		t.Fatalf("Cameras returned %v", gotCams)
	}
	gotDevs, err := USBDevices(screenCtx(t))
	if err != nil {
		t.Fatalf("USBDevices: %v", err)
	}
	// ⛔ DOWN TO bmAttributes, because that is the answer the census exists
	// for: a transport that dropped it would make every UVC camera look
	// readable through an API that cannot read it.
	if len(gotDevs) != 1 || gotDevs[0].BulkVideo() || !gotDevs[0].Viture() {
		t.Fatalf("USBDevices returned %v", gotDevs)
	}
	if got := ChooseRoute(gotCams, gotDevs); got != RouteCamera2 {
		t.Fatalf("ChooseRoute over the wire = %s, want %s", got, RouteCamera2)
	}

	// ⛔ AND IT LEAVES NO CONNECTION BEHIND. A census is a question with an
	// answer; a session kept per call would hold one of the host's threads for
	// the life of the process, and an XR application asks on every attachment.
	waitFor(t, "the census connections to close", func() bool { return fw.live() == 0 })
}

func TestTheCensusWithNoHost(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })
	lookupEnv = func(string) (string, bool) { return "", false }

	if _, err := Cameras(screenCtx(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Cameras with no host: %v, want ErrUnsupported", err)
	}
	if _, err := USBDevices(screenCtx(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("USBDevices with no host: %v, want ErrUnsupported", err)
	}
}

func TestTheCensusWhenNothingListens(t *testing.T) {
	t.Setenv(EnvWallSocket, "xr-census-nothing-listens-here")
	oldBudget, oldPause := dialBudget, dialPause
	dialBudget, dialPause = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { dialBudget, dialPause = oldBudget, oldPause })

	if _, err := Cameras(screenCtx(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Cameras against a dead socket: %v, want ErrUnsupported", err)
	}
}

func TestTheCensusRefusesAnAnswerItCannotDecode(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnCensus(func(p *fakePanel, typ uint8) bool {
		// A body shorter than the four bytes a count needs. An undecodable
		// answer must be an error rather than an empty list: "no external
		// camera here" and "I could not read the answer" are different claims,
		// and the second read as the first sends a command away saying the
		// phone cannot reach the glasses when nobody knows.
		if typ == MsgListCameras {
			p.send(MsgCameras, make([]byte, 2))
		} else {
			p.send(MsgUSBDevices, make([]byte, 2))
		}
		return true
	})
	if _, err := Cameras(screenCtx(t)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("Cameras on an undecodable answer: %v", err)
	}
	if _, err := USBDevices(screenCtx(t)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("USBDevices on an undecodable answer: %v", err)
	}
	waitFor(t, "the connections to close", func() bool { return fw.live() == 0 })
}

func TestTheCensusHonoursItsContext(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnCensus(func(*fakePanel, uint8) bool { return true }) // answer nothing

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := USBDevices(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("USBDevices against a silent host: %v, want DeadlineExceeded", err)
	}
}
