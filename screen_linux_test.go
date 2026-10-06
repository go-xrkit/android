// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

func screenCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestAScreenPutsTheApplicationsOwnPixelsOnTheHostsSide is the end-to-end
// measurement of the output direction, over a real socket and a real shared
// mapping: what the application draws must be the bytes the host finds.
//
// It is the half the live proof on a headset CANNOT make — there is nothing to
// read back from a display an ordinary app does not own, so the only witness
// there is the person wearing the glasses.
func TestAScreenPutsTheApplicationsOwnPixelsOnTheHostsSide(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setDisplays([]Display{glasses})

	s, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 64, Height: 32})
	if err != nil {
		t.Fatalf("ShowOn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if s.DisplayID() != glasses.ID {
		t.Fatalf("the screen is on display %d, want %d", s.DisplayID(), glasses.ID)
	}
	c, err := s.Next(screenCtx(t))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// One known pixel, written through the row rather than through image/draw,
	// so a stride the transport got wrong shows as a pixel in the wrong place
	// rather than as nothing at all.
	row := c.Row(3)
	row[4*5+0], row[4*5+1], row[4*5+2], row[4*5+3] = 0x12, 0x34, 0x56, 0xff
	if err := s.Present(screenCtx(t), c); err != nil {
		t.Fatalf("Present: %v", err)
	}
	waitFor(t, "the host to acknowledge", func() bool { return s.Stats().Acknowledged == 1 })

	p := fw.panel(t)
	if got := p.pixelAt(c.slot, 5, 3); got != 0x123456 {
		t.Fatalf("the host found %#06x at (5,3), want 0x123456", got)
	}
	// And nowhere else: a transport that smeared the frame would also pass a
	// test that only looked where it wrote.
	if got := p.pixelAt(c.slot, 6, 3); got != 0 {
		t.Fatalf("the host found %#06x at (6,3), which nothing wrote", got)
	}
}

func TestScreensAsksTheWallHostAndKeepsOnlyWhatCanCarryAPresentation(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setDisplays([]Display{
		{ID: DefaultDisplayID, Name: "Inner Display", Width: 2152, Height: 2076,
			DensityDPI: 390, RefreshRate: 120, Flags: FlagPresentation},
		glasses,
	})
	got, err := Screens(screenCtx(t))
	if err != nil {
		t.Fatalf("Screens: %v", err)
	}
	if len(got) != 1 || got[0].Name != "VITURE Beast" {
		t.Fatalf("Screens returned %v, want only the glasses", got)
	}
	// ⛔ AND IT MUST NOT LEAVE A CONNECTION BEHIND. A display list is a
	// question with an answer; a session kept open per call would hold one of
	// the host's panel threads for the life of the process.
	waitFor(t, "the listing connection to close", func() bool { return fw.live() == 0 })
}

func TestScreensWithNoHost(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })
	lookupEnv = func(string) (string, bool) { return "", false }

	if _, err := Screens(screenCtx(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Screens with no host: %v, want ErrUnsupported", err)
	}
	if _, err := ShowOn(screenCtx(t), glasses, ScreenOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ShowOn with no host: %v, want ErrUnsupported", err)
	}
}

func TestScreensWhenNothingListens(t *testing.T) {
	t.Setenv(EnvWallSocket, "xr-screen-nothing-listens-here")
	oldBudget, oldPause := dialBudget, dialPause
	dialBudget, dialPause = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { dialBudget, dialPause = oldBudget, oldPause })

	if _, err := Screens(screenCtx(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Screens against a dead socket: %v, want ErrUnsupported", err)
	}
	if _, err := ShowOn(screenCtx(t), glasses, ScreenOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("ShowOn against a dead socket: %v, want ErrUnsupported", err)
	}
}

func TestScreensRefusesADisplayListItCannotDecode(t *testing.T) {
	fw := newFakeWall(t, 0)
	// A body shorter than the four bytes a count needs. A list that will not
	// decode must be an error rather than an empty list: "no glasses here" and
	// "I could not read the answer" are different claims, and the second one
	// read as the first would send a command away saying nothing is plugged in.
	fw.setOnList(func(p *fakePanel) bool {
		p.send(MsgDisplays, make([]byte, 2))
		return true
	})
	if _, err := Screens(screenCtx(t)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("Screens on an undecodable list: %v", err)
	}
}

// Every way the host can answer the screen handshake wrongly.
func TestShowOnRefusesAMalformedHandshake(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(*fakePanel, OpenScreenMessage) bool
		want string
	}{
		{
			name: "a config too short to decode",
			hook: func(p *fakePanel, _ OpenScreenMessage) bool {
				p.send(MsgConfig, make([]byte, 8))
				return true
			},
			want: "config is 8 bytes",
		},
		{
			name: "a config describing no pixels",
			hook: func(p *fakePanel, m OpenScreenMessage) bool {
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Format: FormatRGBA,
					Slots: m.Slots, SlotSize: 1}))
				return true
			},
			want: "0x0 capture",
		},
		{
			// ⛔ THE SIZE IS A CONTRACT. The application draws against the
			// numbers it asked for; a host that quietly presented at another
			// size would scale or clip every frame with nothing to notice here.
			name: "a size other than the one asked for",
			hook: func(p *fakePanel, m OpenScreenMessage) bool {
				m.Width /= 2
				p.openScreenDefault(m)
				return true
			},
			want: "and it announced",
		},
		{
			name: "a buffer that is not the one announced",
			hook: func(p *fakePanel, m OpenScreenMessage) bool {
				stride := m.Width * 4
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
					Stride: stride, Format: FormatRGBA, Slots: m.Slots,
					SlotSize: int64(stride) * int64(m.Height), DisplayID: m.DisplayID}))
				p.lendBuffer(m.Slots, 64)
				return true
			},
			want: "having announced",
		},
		{
			name: "a buffer message too short to decode",
			hook: func(p *fakePanel, m OpenScreenMessage) bool {
				stride := m.Width * 4
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
					Stride: stride, Format: FormatRGBA, Slots: m.Slots,
					SlotSize: int64(stride) * int64(m.Height), DisplayID: m.DisplayID}))
				p.send(MsgBuffer, make([]byte, 3))
				return true
			},
			want: "buffer",
		},
		{
			name: "the host refusing the display outright",
			hook: func(p *fakePanel, m OpenScreenMessage) bool {
				p.send(MsgError, EncodeError(ErrorMessage{
					Code: codeNotPresentable, Op: "getDisplay",
					Detail: "display 12 has no FLAG_PRESENTATION",
				}))
				return true
			},
			want: "FLAG_PRESENTATION",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFakeWall(t, 0)
			fw.setOnScreen(tc.hook)

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := ShowOn(ctx, glasses, ScreenOptions{Width: 64, Height: 32})
			if err == nil {
				t.Fatal("ShowOn accepted a malformed handshake")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A host that refuses the display maps onto the sentinel, so a caller can use
// errors.Is without knowing Android's vocabulary.
func TestAHostRefusalMapsOntoErrNotPresentable(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnScreen(func(p *fakePanel, m OpenScreenMessage) bool {
		p.send(MsgError, EncodeError(ErrorMessage{Code: codeNotPresentable,
			Op: "getDisplay", Detail: "it was unplugged"}))
		return true
	})
	_, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 64, Height: 32})
	if !errors.Is(err, ErrNotPresentable) {
		t.Fatalf("ShowOn reported %v, want ErrNotPresentable", err)
	}
}

func TestShowOnHonoursItsContext(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnScreen(func(*fakePanel, OpenScreenMessage) bool { return true }) // answer nothing

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := ShowOn(ctx, glasses, ScreenOptions{Width: 64, Height: 32})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ShowOn against a silent host: %v, want DeadlineExceeded", err)
	}
}

func TestShowOnWhenTheHostVanishesMidHandshake(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnScreen(func(p *fakePanel, m OpenScreenMessage) bool {
		stride := m.Width * 4
		p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
			Stride: stride, Format: FormatRGBA, Slots: m.Slots,
			SlotSize: int64(stride) * int64(m.Height), DisplayID: m.DisplayID}))
		_ = p.conn.Close()
		return true
	})
	_, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 64, Height: 32})
	if err == nil {
		t.Fatal("ShowOn survived the host going away mid-handshake")
	}
}

func TestShowOnRefusesAnUnmappableBuffer(t *testing.T) {
	newFakeWall(t, 0)
	old := mmap
	t.Cleanup(func() { mmap = old })
	mmap = func(int, int64, int, int, int) ([]byte, error) { return nil, syscall.ENOMEM }

	_, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 64, Height: 32})
	if !errors.Is(err, syscall.ENOMEM) {
		t.Fatalf("ShowOn with an unmappable buffer: %v", err)
	}
	if !strings.Contains(err.Error(), "for writing") {
		t.Fatalf("the error says %q, which does not say which direction failed", err)
	}
}

// ⛔ THE MAPPING IS WRITABLE, which is the whole difference from a capture's.
// A read-only mapping would fault on the first frame an application drew, and
// the fault is a SIGSEGV rather than an error anybody can catch.
func TestTheScreensMappingIsWritable(t *testing.T) {
	newFakeWall(t, 0)
	s, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 32, Height: 16})
	if err != nil {
		t.Fatalf("ShowOn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	c, err := s.Next(screenCtx(t))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	for i := range c.Pix {
		c.Pix[i] = 0xAB
	}
}

func TestClosingAScreenDropsTheHostConnection(t *testing.T) {
	fw := newFakeWall(t, 0)
	s, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 32, Height: 16})
	if err != nil {
		t.Fatalf("ShowOn: %v", err)
	}
	waitFor(t, "the host to be serving the screen", func() bool { return fw.live() == 1 })
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "the host to drop the screen", func() bool { return fw.live() == 0 })
}

func TestAScreenNoticesTheHostStopping(t *testing.T) {
	fw := newFakeWall(t, 0)
	s, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 32, Height: 16})
	if err != nil {
		t.Fatalf("ShowOn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The host saying the Presentation went away — the cable pulled, the
	// service killed. It must reach the screen as the host's OWN words.
	fw.panel(t).send(MsgStopped, EncodeStopped(StoppedMessage{
		Reason: StopSystem, Detail: "the display was removed"}))
	waitFor(t, "the screen to notice", func() bool { return s.Err() != nil })
	if !strings.Contains(s.Err().Error(), "the display was removed") {
		t.Fatalf("Err is %v, which does not carry the host's reason", s.Err())
	}
}

func TestAScreenSurvivesAnAcknowledgementItCannotDecode(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnPresent(func(p *fakePanel, m PresentMessage) bool {
		p.send(MsgPresented, make([]byte, 3))
		return false
	})
	s, err := ShowOn(screenCtx(t), glasses, ScreenOptions{Width: 32, Height: 16})
	if err != nil {
		t.Fatalf("ShowOn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	c, err := s.Next(screenCtx(t))
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := s.Present(screenCtx(t), c); err != nil {
		t.Fatalf("Present: %v", err)
	}
	// ⛔ IT ENDS THE SCREEN RATHER THAN IGNORING IT. An acknowledgement names a
	// slot; a screen that stopped believing them would stall on the next frame
	// with nothing said.
	waitFor(t, "the screen to end", func() bool { return s.Err() != nil })
	if !errors.Is(s.Err(), ErrShortPayload) {
		t.Fatalf("Err is %v, want the decode failure", s.Err())
	}
}

// A host that takes the question and never answers must not hang a command
// looking for the glasses: the cable it was started over is the one being
// moved, and nobody is there to interrupt it.
func TestScreensHonoursItsContext(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnList(func(*fakePanel) bool { return true }) // answer nothing

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := Screens(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Screens against a silent host: %v, want DeadlineExceeded", err)
	}
}
