// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive [Screen] over a feed built here rather than over a socket,
// which is what lets the whole of its behaviour be measured on EVERY GOOS
// rather than on the one that has a host. The transport is tested separately in
// screen_linux_test.go, against a real socket and a real shared mapping.

// testFeed is a host in a struct: it records what was presented and
// acknowledges on demand.
type testFeed struct {
	acks chan PresentedMessage
	fail chan error

	mu        sync.Mutex
	presented []PresentMessage
	closed    int
	err       error  // what present reports
	closeErr  error  // what close reports
	hold      bool   // withhold acknowledgements
	buf       []byte // the mapping, kept so a test can read what was drawn
}

func newTestFeed(t *testing.T, w, h, slots int) (*testFeed, screenFeed) {
	t.Helper()
	stride := w * 4
	slotSize := int64(stride) * int64(h)
	f := &testFeed{
		acks: make(chan PresentedMessage, slots),
		fail: make(chan error, 1),
		buf:  make([]byte, int64(slots)*slotSize),
	}
	sf := screenFeed{
		buf: f.buf, stride: stride, slotSize: slotSize, slots: slots,
		width: w, height: h, displayID: 12,
		acks: f.acks, fail: f.fail,
		present: f.present,
		close:   f.close,
	}
	return f, sf
}

func (f *testFeed) present(m PresentMessage) error {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
		f.mu.Unlock()
		return err
	}
	f.presented = append(f.presented, m)
	hold := f.hold
	f.mu.Unlock()
	if !hold {
		f.acks <- PresentedMessage{Seq: m.Seq, Slot: m.Slot}
	}
	return nil
}

func (f *testFeed) close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return f.closeErr
}

func (f *testFeed) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.presented)
}

// release acknowledges everything held back, which is how a test lets a blocked
// Next through.
func (f *testFeed) release() {
	f.mu.Lock()
	ms := append([]PresentMessage(nil), f.presented...)
	f.hold = false
	f.mu.Unlock()
	for _, m := range ms {
		f.acks <- PresentedMessage{Seq: m.Seq, Slot: m.Slot}
	}
}

// waitFor polls until cond holds or the test's patience runs out. A screen
// acknowledges on another goroutine, so a counter read straight after a Present
// is a race rather than a measurement.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// glasses is the display the live measurement found on a Pixel 11 Pro Fold, so
// the tests present on the same thing the device did.
var glasses = Display{
	ID: 12, Name: "VITURE Beast", Width: 1920, Height: 1080,
	DensityDPI: 110, RefreshRate: 60, Flags: FlagPresentation,
}

func TestAScreenCarriesWhatWasDrawnIntoIt(t *testing.T) {
	f, sf := newTestFeed(t, 64, 32, 3)
	s := newScreen(sf, ScreenOptions{Width: 64, Height: 32, QueueDepth: 3})
	defer func() {
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}()

	c, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if c.Width != 64 || c.Height != 32 || c.Stride != 256 {
		t.Fatalf("canvas is %dx%d stride %d, want 64x32 stride 256", c.Width, c.Height, c.Stride)
	}
	// Painted through image/draw, which is how an application will use this.
	draw.Draw(c.RGBA(), image.Rect(0, 0, 4, 4),
		&image.Uniform{color.RGBA{G: 0xff, A: 0xff}}, image.Point{}, draw.Src)
	if err := s.Present(context.Background(), c); err != nil {
		t.Fatalf("Present: %v", err)
	}

	waitFor(t, "the frame to be acknowledged", func() bool { return s.Stats().Acknowledged == 1 })
	f.mu.Lock()
	m := f.presented[0]
	f.mu.Unlock()
	if m.Seq != 1 || m.Width != 64 || m.Height != 32 || m.Stride != 256 {
		t.Fatalf("presented %+v, want seq 1 and 64x32 stride 256", m)
	}
	// ⛔ THE PIXELS, NOT THE COUNT. A screen that presents frames of nothing is
	// this feature's silent failure, and the count cannot tell the difference.
	off := int64(m.Slot) * sf.slotSize
	if got := f.buf[off : off+4]; got[0] != 0 || got[1] != 0xff || got[2] != 0 || got[3] != 0xff {
		t.Fatalf("the first pixel reached the buffer as %v, want opaque green", got)
	}
	if st := s.Stats(); st.Presented != 1 || st.Waited != 0 {
		t.Fatalf("stats are %s, want one presented and no waits", st)
	}
}

func TestEveryPresentedSlotComesBack(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	// Twice round the ring, so a slot that was never freed would show as a
	// wait that never ended rather than as a count being one short.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 7; i++ {
		c, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if err := s.Present(ctx, c); err != nil {
			t.Fatalf("Present %d: %v", i, err)
		}
	}
	waitFor(t, "every frame to be acknowledged", func() bool { return s.Stats().Acknowledged == 7 })
	if n := f.count(); n != 7 {
		t.Fatalf("the host saw %d frames, want 7", n)
	}
}

func TestNextWaitsWhenEverySlotIsInFlight(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	f.mu.Lock()
	f.hold = true
	f.mu.Unlock()
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		c, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if err := s.Present(ctx, c); err != nil {
			t.Fatalf("Present %d: %v", i, err)
		}
	}
	// The fourth has nowhere to go until the host answers.
	done := make(chan error, 1)
	go func() {
		_, err := s.Next(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Next returned %v with every slot in flight; it must wait", err)
	case <-time.After(50 * time.Millisecond):
	}
	f.release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Next after the host caught up: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next never woke after the host acknowledged three frames")
	}
	if st := s.Stats(); st.Waited != 1 {
		t.Fatalf("stats are %s, want exactly one wait", st)
	}
}

func TestAWaitIsAbandonedWithItsContext(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	f.mu.Lock()
	f.hold = true
	f.mu.Unlock()
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	for i := 0; i < 3; i++ {
		c, _ := s.Next(context.Background())
		if err := s.Present(context.Background(), c); err != nil {
			t.Fatalf("Present %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next on an expired context reported %v, want a deadline", err)
	}
}

func TestACanvasCannotBePresentedTwice(t *testing.T) {
	_, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	c, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if err := s.Present(context.Background(), c); err != nil {
		t.Fatalf("Present: %v", err)
	}
	// ⛔ THE SECOND ONE WOULD FREE A SLOT THAT IS IN USE, and the next frame
	// would be drawn over one being blitted. It is refused, not tolerated.
	err = s.Present(context.Background(), c)
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("presenting the same canvas twice reported %v, want ErrInvalidOption", err)
	}
	if !strings.Contains(err.Error(), "not one this screen is lending") {
		t.Fatalf("the refusal says %q, which does not say what is wrong", err)
	}
}

func TestACanvasFromNowhereIsRefused(t *testing.T) {
	_, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	// A slot number reaches the host as an offset into shared memory, so one
	// this screen never lent must not get there.
	err := s.Present(context.Background(), Canvas{slot: 2, token: 99})
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("presenting a made-up canvas reported %v, want ErrInvalidOption", err)
	}
}

func TestAFailedPresentGivesTheSlotBack(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	boom := errors.New("the socket went away")
	f.mu.Lock()
	f.err = boom
	f.mu.Unlock()

	ctx := context.Background()
	// Three failures in a row: with the slot lost each time, the fourth Next
	// would block forever rather than report anything.
	for i := 0; i < 4; i++ {
		c, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if err := s.Present(ctx, c); !errors.Is(err, boom) {
			t.Fatalf("Present %d reported %v, want the transport's own error", i, err)
		}
	}
	// ⛔ AND THE COUNTER MUST NOT SAY THEY ARRIVED. A frame the host never got
	// is not an acknowledged frame.
	if st := s.Stats(); st.Acknowledged != 0 {
		t.Fatalf("stats are %s, want nothing acknowledged", st)
	}
}

func TestAHostThatGoesAwayEndsTheScreen(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	boom := errors.New("the glasses were unplugged")
	f.fail <- boom
	waitFor(t, "the screen to notice", func() bool { return s.Err() != nil })
	if !errors.Is(s.Err(), boom) {
		t.Fatalf("Err is %v, want the host's own failure", s.Err())
	}
	if _, err := s.Next(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Next after the host went away reported %v", err)
	}
	c := Canvas{slot: 0}
	if err := s.Present(context.Background(), c); !errors.Is(err, boom) {
		t.Fatalf("Present after the host went away reported %v", err)
	}
}

func TestAClosedAckChannelEndsTheScreen(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	close(f.acks)
	waitFor(t, "the screen to notice", func() bool { return s.Err() != nil })
	if !errors.Is(s.Err(), ErrClosed) {
		t.Fatalf("Err is %v, want ErrClosed", s.Err())
	}
}

func TestAnAcknowledgementForASlotNobodyHasIsIgnored(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	// ⛔ FREEING IT WOULD HAND THE SAME SLOT OUT TWICE, which is a tear with no
	// trace. The host is the side this process cannot verify.
	f.acks <- PresentedMessage{Seq: 7, Slot: 1}
	f.acks <- PresentedMessage{Seq: 8, Slot: 99}
	time.Sleep(20 * time.Millisecond)
	if st := s.Stats(); st.Acknowledged != 0 {
		t.Fatalf("stats are %s after two bogus acknowledgements, want none counted", st)
	}
	// Every slot is still exactly once on the free list.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		c, err := s.Next(ctx)
		if err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
		if seen[c.slot] {
			t.Fatalf("slot %d was lent twice", c.slot)
		}
		seen[c.slot] = true
	}
}

func TestClosingAScreenIsIdempotentAndReportsTheTransport(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	boom := errors.New("unmapping failed")
	f.closeErr = boom
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})

	if err := s.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close reported %v, want the transport's error", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("the second Close reported %v, want nil", err)
	}
	if f.closed != 1 {
		t.Fatalf("the transport was closed %d times, want 1", f.closed)
	}
	if _, err := s.Next(context.Background()); !errors.Is(err, ErrScreenClosed) {
		t.Fatalf("Next after Close reported %v", err)
	}
	if err := s.Present(context.Background(), Canvas{}); !errors.Is(err, ErrScreenClosed) {
		t.Fatalf("Present after Close reported %v", err)
	}
}

func TestPresentRefusesAnExpiredContextBeforeAnythingElse(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	defer func() { _ = s.Close() }()

	c, err := s.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Present(ctx, c); !errors.Is(err, context.Canceled) {
		t.Fatalf("Present on a cancelled context reported %v", err)
	}
	if n := f.count(); n != 0 {
		t.Fatalf("the host saw %d frames from a cancelled Present, want none", n)
	}
}

func TestAClosedScreenStopsANextThatIsWaiting(t *testing.T) {
	f, sf := newTestFeed(t, 32, 16, 3)
	f.hold = true
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})
	for i := 0; i < 3; i++ {
		c, _ := s.Next(context.Background())
		if err := s.Present(context.Background(), c); err != nil {
			t.Fatalf("Present %d: %v", i, err)
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Next(context.Background())
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = s.Close()
	select {
	case err := <-done:
		if !errors.Is(err, ErrScreenClosed) {
			t.Fatalf("the waiting Next reported %v, want ErrScreenClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("closing the screen left a Next waiting forever")
	}
}

func TestScreenReportsWhatItIs(t *testing.T) {
	_, sf := newTestFeed(t, 1920, 1080, 3)
	opts := ScreenOptions{Width: 1920, Height: 1080, QueueDepth: 3}
	s := newScreen(sf, opts)
	defer func() { _ = s.Close() }()

	if s.DisplayID() != 12 {
		t.Fatalf("DisplayID is %d, want 12", s.DisplayID())
	}
	if w, h := s.Size(); w != 1920 || h != 1080 {
		t.Fatalf("Size is %dx%d, want 1920x1080", w, h)
	}
	if s.Stride() != 1920*4 {
		t.Fatalf("Stride is %d, want %d", s.Stride(), 1920*4)
	}
	if s.Options() != opts {
		t.Fatalf("Options are %v, want %v", s.Options(), opts)
	}
	for _, want := range []string{"display 12", "1920x1080", "7680-byte rows", "0 presented"} {
		if !strings.Contains(s.String(), want) {
			t.Fatalf("String is %q, which does not say %q", s.String(), want)
		}
	}
}

func TestScreenOptionsFillThemselvesInFromTheDisplay(t *testing.T) {
	got, err := ScreenOptions{}.resolve(glasses)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := ScreenOptions{Width: 1920, Height: 1080, QueueDepth: DefaultQueueDepth}
	if got != want {
		t.Fatalf("the zero options against %s resolved to %v, want %v", glasses, got, want)
	}
	// What is given is kept: a screen rendered deliberately small is a choice.
	got, err = ScreenOptions{Width: 960, Height: 540, QueueDepth: 4}.resolve(glasses)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != (ScreenOptions{Width: 960, Height: 540, QueueDepth: 4}) {
		t.Fatalf("resolve changed options that were given: %v", got)
	}
	if s := got.String(); s != "960x540, 4 slots" {
		t.Fatalf("String is %q", s)
	}
}

func TestScreenOptionsRefuseWhatCannotBeAFrame(t *testing.T) {
	for _, c := range []struct {
		name string
		o    ScreenOptions
		says string
	}{
		{"too small", ScreenOptions{Width: 8, Height: 8, QueueDepth: 3}, "too small"},
		{"too wide", ScreenOptions{Width: MaxDimension + 1, Height: 1080, QueueDepth: 3}, "pixel limit"},
		{"too many pixels", ScreenOptions{Width: 30000, Height: 30000, QueueDepth: 3}, "beyond the"},
		{"too few slots", ScreenOptions{Width: 64, Height: 64, QueueDepth: 1}, "frame slots"},
		{"too many slots", ScreenOptions{Width: 64, Height: 64, QueueDepth: MaxQueueDepth + 1}, "frame slots"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := c.o.Validate()
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Validate reported %v, want ErrInvalidOption", err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("the refusal says %q, which does not mention %q", err, c.says)
			}
			// resolve must refuse the same thing: it is what ShowOn calls.
			if _, err := c.o.resolve(glasses); !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("resolve reported %v", err)
			}
		})
	}
}

func TestACanvasIsAnImageAndASetOfRows(t *testing.T) {
	c := Canvas{Pix: make([]byte, 8*40), Stride: 40, Width: 8, Height: 8}
	img := c.RGBA()
	if img.Rect != image.Rect(0, 0, 8, 8) || img.Stride != 40 {
		t.Fatalf("RGBA is %v stride %d, want an 8x8 image of stride 40", img.Rect, img.Stride)
	}
	// ⛔ THE SAME MEMORY, NOT A COPY. If RGBA copied, every frame an
	// application drew would be thrown away and the screen would show the
	// buffer's leftovers.
	img.Set(1, 1, color.RGBA{R: 0xff, A: 0xff})
	if c.Pix[40+4] != 0xff {
		t.Fatalf("drawing through RGBA did not reach the canvas's own bytes")
	}

	row := c.Row(1)
	if len(row) != 32 || cap(row) != 32 {
		t.Fatalf("Row is %d bytes of %d capacity, want 32 of 32", len(row), cap(row))
	}
	// ⛔ THE CAPACITY STOPS AT THE ROW. The padding a stride leaves belongs to
	// the host; a span that ran into it would corrupt the next row silently.
	row = append(row, 1, 2, 3, 4)
	if c.Pix[40+32] != 0 {
		t.Fatalf("appending past a row reached the stride padding")
	}
	for _, y := range []int{-1, 8, 99} {
		if c.Row(y) != nil {
			t.Fatalf("Row(%d) is outside the frame and must be nil", y)
		}
	}
	if (Canvas{Width: 8, Height: 8, Stride: 4}).Row(0) != nil {
		t.Fatal("a stride too small for the width describes no row")
	}
	if s := c.String(); !strings.Contains(s, "8x8") || !strings.Contains(s, "40-byte") {
		t.Fatalf("String is %q", s)
	}
}

func TestOnlyADisplayThePlatformFlaggedTakesAPresentation(t *testing.T) {
	if err := checkPresentable(glasses); err != nil {
		t.Fatalf("the glasses were refused: %v", err)
	}
	// The built-in panel: flagged or not, an ordinary app may not present over
	// the launcher, and the refusal has to SAY that rather than name a flag.
	builtin := Display{ID: DefaultDisplayID, Name: "Inner Display", Width: 2152, Height: 2076}
	err := checkPresentable(builtin)
	if !errors.Is(err, ErrNotPresentable) || !strings.Contains(err.Error(), "built-in panel") {
		t.Fatalf("the built-in panel was refused with %v", err)
	}
	other := Display{ID: 7, Name: "something else", Width: 800, Height: 600}
	err = checkPresentable(other)
	if !errors.Is(err, ErrNotPresentable) || !strings.Contains(err.Error(), "FLAG_PRESENTATION") {
		t.Fatalf("an unflagged display was refused with %v", err)
	}
}

func TestPresentableKeepsTheGlassesAndDropsTheBuiltInPanel(t *testing.T) {
	// ⛔ THE FLAG ALONE IS NOT THE ANSWER: the platform sets FLAG_PRESENTATION
	// on the default display of some devices, and an application that believed
	// it would ask for a window over the launcher.
	in := []Display{
		{ID: DefaultDisplayID, Name: "Inner Display", Flags: FlagPresentation},
		{ID: 3, Name: "nothing special"},
		glasses,
	}
	got := presentable(in)
	if len(got) != 1 || got[0].ID != glasses.ID {
		t.Fatalf("presentable kept %v, want only the glasses", got)
	}
}

func TestShowOnRefusesBeforeItDialsAnything(t *testing.T) {
	ctx := context.Background()
	// ⛔ NO ROUND TRIP FOR AN ANSWER ALREADY IN HAND. The display list said
	// whether this display takes a presentation; asking the host would come
	// back saying less, and off a device it would report ErrUnsupported and
	// hide the real reason.
	builtin := Display{ID: DefaultDisplayID, Width: 2152, Height: 2076}
	if _, err := ShowOn(ctx, builtin, ScreenOptions{}); !errors.Is(err, ErrNotPresentable) {
		t.Fatalf("ShowOn on the built-in panel reported %v", err)
	}
	if _, err := ShowOn(ctx, glasses, ScreenOptions{Width: 4}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("ShowOn with a 4-pixel frame reported %v", err)
	}
}

func TestScreenStatsSayWhatHappened(t *testing.T) {
	s := ScreenStats{Presented: 1800, Acknowledged: 1799, Waited: 12}
	if got := s.String(); got != "1800 presented, 1799 acknowledged, 12 waits" {
		t.Fatalf("String is %q", got)
	}
}

// ⛔ CLOSE LANDING BETWEEN THE CHECK AND THE SLOT must not hand out a canvas:
// the memory under it is about to be unmapped, and a write there is a SIGSEGV
// rather than an error anybody can catch.
func TestNextRefusesACloseThatLandsWhileItWaits(t *testing.T) {
	_, sf := newTestFeed(t, 32, 16, 3)
	s := newScreen(sf, ScreenOptions{Width: 32, Height: 16, QueueDepth: 3})

	old := afterTakingASlot
	t.Cleanup(func() { afterTakingASlot = old })
	afterTakingASlot = func() { _ = s.Close() }

	if _, err := s.Next(context.Background()); !errors.Is(err, ErrScreenClosed) {
		t.Fatalf("Next with a Close landing mid-call reported %v, want ErrScreenClosed", err)
	}
}

// ⛔ SHOWON'S LAST LINE RUNS ON EVERY PLATFORM OR ON NONE. With the transport
// stubbed out, the refusals and the handover are portable and are measured
// identically everywhere — which is also how the off-Android stub stays honest
// rather than being a line nobody has ever executed.
func TestShowOnHandsBackWhatTheTransportGaveIt(t *testing.T) {
	_, sf := newTestFeed(t, 1920, 1080, 3)
	old := openScreen
	t.Cleanup(func() { openScreen = old })
	var asked ScreenOptions
	openScreen = func(_ context.Context, d Display, o ScreenOptions) (screenFeed, error) {
		asked = o
		return sf, nil
	}
	s, err := ShowOn(context.Background(), glasses, ScreenOptions{})
	if err != nil {
		t.Fatalf("ShowOn: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// The transport is asked for the RESOLVED options, not the zero ones: a
	// host handed Width 0 would make a display of nothing.
	want := ScreenOptions{Width: 1920, Height: 1080, QueueDepth: DefaultQueueDepth}
	if asked != want {
		t.Fatalf("the transport was asked for %v, want %v", asked, want)
	}
	if s.Options() != want {
		t.Fatalf("the screen reports %v, want %v", s.Options(), want)
	}
}
