// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// stubOpener is a transport that hands back a Stream without a host, so the
// wall's own bookkeeping can be driven on any platform.
func stubOpener(id int) func(context.Context, DisplaySpec) (feed, error) {
	return func(context.Context, DisplaySpec) (feed, error) {
		// The releaser is a no-op rather than the real Stream.Close: this
		// Stream was never built by a transport, and closing it for real is a
		// nil dereference on the lane where Stream has a session in it.
		return feed{stream: &Stream{}, id: id, close: func() error { return nil }}, nil
	}
}

func failingOpener(err error) func(context.Context, DisplaySpec) (feed, error) {
	return func(context.Context, DisplaySpec) (feed, error) { return feed{}, err }
}

func goodSpec() DisplaySpec {
	return DisplaySpec{Width: 640, Height: 480, Content: Sentinel{Label: "t"}}
}

func TestNewWallLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		max  int
		want int // 0 means "must fail"
	}{
		{"zero takes the default", 0, DefaultMaxDisplays},
		{"one", 1, 1},
		{"the ceiling itself", MaxDisplays, MaxDisplays},
		{"negative", -1, 0},
		{"past the ceiling", MaxDisplays + 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, err := NewWall(tc.max)
			if tc.want == 0 {
				if !errors.Is(err, ErrTooManyDisplays) {
					t.Fatalf("NewWall(%d) error = %v, want ErrTooManyDisplays", tc.max, err)
				}
				if w != nil {
					t.Fatalf("NewWall(%d) returned a wall alongside its refusal", tc.max)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewWall(%d): %v", tc.max, err)
			}
			if got := w.Max(); got != tc.want {
				t.Fatalf("Max = %d, want %d", got, tc.want)
			}
			if got := w.Len(); got != 0 {
				t.Fatalf("a fresh wall holds %d displays, want 0", got)
			}
		})
	}
}

// The refusal is the whole safety of this feature, so it is asserted in BOTH
// directions: exactly the limit must succeed, and one more must fail, and the
// failure must create nothing.
func TestWallRefusesPastItsLimitAndAcceptsUpToIt(t *testing.T) {
	t.Parallel()
	const max = 3
	w, err := NewWall(max)
	if err != nil {
		t.Fatalf("NewWall: %v", err)
	}
	var opened int
	w.opener = func(context.Context, DisplaySpec) (feed, error) {
		opened++
		return feed{stream: &Stream{}, id: 100 + opened, close: func() error { return nil }}, nil
	}

	// Up to the limit: every one succeeds.
	ds := make([]*OwnedDisplay, 0, max)
	for i := 0; i < max; i++ {
		d, err := w.Open(context.Background(), goodSpec())
		if err != nil {
			t.Fatalf("Open %d of %d: %v", i+1, max, err)
		}
		ds = append(ds, d)
		if got := w.Len(); got != i+1 {
			t.Fatalf("after %d opens Len = %d", i+1, got)
		}
	}

	// One more: refused, and the host was never asked.
	before := opened
	d, err := w.Open(context.Background(), goodSpec())
	if !errors.Is(err, ErrTooManyDisplays) {
		t.Fatalf("Open past the limit: error = %v, want ErrTooManyDisplays", err)
	}
	if d != nil {
		t.Fatal("Open past the limit returned a display alongside its refusal")
	}
	if opened != before {
		t.Fatalf("Open past the limit still asked the host: %d calls, want %d", opened, before)
	}
	if got := w.Len(); got != max {
		t.Fatalf("a refusal changed the wall: Len = %d, want %d", got, max)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("holds %d of at most %d", max, max)) {
		t.Fatalf("the refusal does not say what it is refusing: %v", err)
	}

	// Closing one makes room for exactly one more.
	if err := ds[0].Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Len(); got != max-1 {
		t.Fatalf("after a close Len = %d, want %d", got, max-1)
	}
	if _, err := w.Open(context.Background(), goodSpec()); err != nil {
		t.Fatalf("Open after making room: %v", err)
	}
	if _, err := w.Open(context.Background(), goodSpec()); !errors.Is(err, ErrTooManyDisplays) {
		t.Fatalf("Open past the limit again: error = %v, want ErrTooManyDisplays", err)
	}
}

// A wall that reserved a slot and then failed to open must not keep the slot,
// or a run of transport failures would silently exhaust it.
func TestWallGivesTheSlotBackWhenTheHostRefuses(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(1)
	boom := errors.New("no host")
	w.opener = failingOpener(boom)
	for i := 0; i < 4; i++ {
		if _, err := w.Open(context.Background(), goodSpec()); !errors.Is(err, boom) {
			t.Fatalf("Open %d: error = %v, want %v", i, err, boom)
		}
		if got := w.Len(); got != 0 {
			t.Fatalf("a failed Open kept a slot: Len = %d", got)
		}
	}
	w.opener = stubOpener(7)
	if _, err := w.Open(context.Background(), goodSpec()); err != nil {
		t.Fatalf("Open after four failures: %v", err)
	}
}

func TestWallOpenRefusesAnInvalidSpec(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(0)
	w.opener = failingOpener(errors.New("the host must not be reached"))
	if _, err := w.Open(context.Background(), DisplaySpec{}); !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("Open with a zero spec: error = %v, want ErrInvalidOption", err)
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("an invalid spec reserved a slot: Len = %d", got)
	}
}

func TestWallCloseIsIdempotentAndReleasesEverything(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(4)
	w.opener = stubOpener(9)
	for i := 0; i < 4; i++ {
		if _, err := w.Open(context.Background(), goodSpec()); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("after Close the wall holds %d displays", got)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := w.Open(context.Background(), goodSpec()); !errors.Is(err, ErrWallClosed) {
		t.Fatalf("Open after Close: error = %v, want ErrWallClosed", err)
	}
}

// Close must report a failure without abandoning the displays after it.
func TestWallCloseReportsTheFirstFailureAndStillClosesTheRest(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(3)
	w.opener = stubOpener(1)
	for i := 0; i < 3; i++ {
		if _, err := w.Open(context.Background(), goodSpec()); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	// Make the middle one fail on close. Wall.Close must report it AND still
	// close the rest: a wall that abandons displays after the first failure
	// leaks exactly the resource this type exists to bound.
	boom := errors.New("the platform refused to release it")
	w.mu.Lock()
	ds := make([]*OwnedDisplay, 0, len(w.open))
	for d := range w.open {
		ds = append(ds, d)
	}
	w.mu.Unlock()
	var closed int
	for i, d := range ds {
		if i == 1 {
			d.closer = func() error { closed++; return boom }
			continue
		}
		d.closer = func() error { closed++; return nil }
	}

	err := w.Close()
	if !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want the display's own failure", err)
	}
	if closed != len(ds) {
		t.Fatalf("Close stopped after the failure: closed %d of %d", closed, len(ds))
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Close left %d displays", got)
	}
}

func TestWallIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(MaxDisplays)
	w.opener = stubOpener(2)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var refused int
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := w.Open(context.Background(), goodSpec())
			switch {
			case errors.Is(err, ErrTooManyDisplays):
				mu.Lock()
				refused++
				mu.Unlock()
			case err != nil:
				t.Errorf("Open: %v", err)
			default:
				_ = d.Close()
			}
		}()
	}
	wg.Wait()
	if refused == 0 {
		t.Skip("no contention arose; the limit was never reached")
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("every opened display was closed but Len = %d", got)
	}
}

func TestOwnedDisplayCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(2)
	w.opener = stubOpener(42)
	d, err := w.Open(context.Background(), goodSpec())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := d.ID(); got != 42 {
		t.Fatalf("ID = %d, want 42", got)
	}
	if got := d.Spec().DensityDPI; got != DefaultDensityDPI {
		t.Fatalf("Spec().DensityDPI = %d, want the default %d", got, DefaultDensityDPI)
	}
	if got := d.Spec().QueueDepth; got != DefaultQueueDepth {
		t.Fatalf("Spec().QueueDepth = %d, want the default %d", got, DefaultQueueDepth)
	}
	if s := d.String(); !strings.Contains(s, "owned display 42") || !strings.Contains(s, "640x480") {
		t.Fatalf("String = %q", s)
	}
	for i := 0; i < 3; i++ {
		if err := d.Close(); err != nil {
			t.Fatalf("Close %d: %v", i, err)
		}
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Len = %d after closing the only display", got)
	}
}

// A display whose Open failed after the slot was reserved has no Stream, and
// closing it must still give the slot back rather than panic.
func TestOwnedDisplayCloseWithNoStream(t *testing.T) {
	t.Parallel()
	w, _ := NewWall(1)
	d := &OwnedDisplay{wall: w} // no closer: Open failed after reserving
	w.mu.Lock()
	w.open[d] = struct{}{}
	w.mu.Unlock()
	if got := w.Len(); got != 1 {
		t.Fatalf("Len = %d", got)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Len = %d after Close", got)
	}
}

func TestDisplaySpecValidate(t *testing.T) {
	t.Parallel()
	base := DisplaySpec{Width: 640, Height: 480, DensityDPI: 320,
		QueueDepth: DefaultQueueDepth, Content: Sentinel{}}
	mut := func(f func(*DisplaySpec)) DisplaySpec {
		s := base
		f(&s)
		return s
	}
	for _, tc := range []struct {
		name string
		spec DisplaySpec
		want string // "" means valid
	}{
		{"valid", base, ""},
		{"too narrow", mut(func(s *DisplaySpec) { s.Width = MinDisplayEdge - 1 }), "too small"},
		{"too short", mut(func(s *DisplaySpec) { s.Height = 0 }), "too small"},
		{"negative", mut(func(s *DisplaySpec) { s.Width = -1 }), "too small"},
		{"too wide", mut(func(s *DisplaySpec) { s.Width = MaxDimension + 1 }), "beyond"},
		{"too tall", mut(func(s *DisplaySpec) { s.Height = MaxDimension + 1 }), "beyond"},
		{"no density", mut(func(s *DisplaySpec) { s.DensityDPI = 0 }), "density"},
		{"absurd density", mut(func(s *DisplaySpec) { s.DensityDPI = 8193 }), "density"},
		{"too few slots", mut(func(s *DisplaySpec) { s.QueueDepth = MinQueueDepth - 1 }), "frame slots"},
		{"too many slots", mut(func(s *DisplaySpec) { s.QueueDepth = MaxQueueDepth + 1 }), "frame slots"},
		{"no content", mut(func(s *DisplaySpec) { s.Content = nil }), "no Content"},
		{"bad content", mut(func(s *DisplaySpec) { s.Content = Web{} }), "URL is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.spec.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrInvalidOption) {
				t.Fatalf("Validate error = %v, want ErrInvalidOption", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestDisplaySpecResolveFillsDefaults(t *testing.T) {
	t.Parallel()
	got, err := DisplaySpec{Width: 100, Height: 100, Content: Sentinel{}}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.DensityDPI != DefaultDensityDPI || got.QueueDepth != DefaultQueueDepth {
		t.Fatalf("resolve = %+v, want the defaults filled in", got)
	}
	if _, err := (DisplaySpec{Width: 1, Height: 1, Content: Sentinel{}}).resolve(); err == nil {
		t.Fatal("resolve accepted a 1x1 display")
	}
}

func TestDisplaySpecString(t *testing.T) {
	t.Parallel()
	s := DisplaySpec{Width: 640, Height: 480, DensityDPI: 320, Content: Web{URL: "https://x"}}
	if got, want := s.String(), "640x480 @320dpi web https://x"; got != want {
		t.Fatalf("String = %q, want %q", got, want)
	}
	if got := (DisplaySpec{}).String(); !strings.Contains(got, "no content") {
		t.Fatalf("String with no content = %q", got)
	}
}

func TestWebContent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://example.invalid/page", true},
		{"http://example.invalid/page", true},
		{"data:text/html,<b>hi</b>", true},
		{"", false},
		{"ftp://example.invalid/x", false},
		{"example.invalid", false},
		{"data" + strings.Repeat("x", MaxPayload), false},
	} {
		err := Web{URL: tc.url}.Validate()
		if tc.ok != (err == nil) {
			t.Fatalf("Web{%.30q}.Validate() = %v, want ok=%v", tc.url, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrInvalidOption) {
			t.Fatalf("Web{%.30q}.Validate() = %v, want ErrInvalidOption", tc.url, err)
		}
	}
	w := Web{URL: "https://example.invalid/page"}
	if w.kind() != contentWeb || w.payload() != w.URL {
		t.Fatalf("kind/payload = %d/%q", w.kind(), w.payload())
	}
	if got := w.Describe(); got != "web https://example.invalid/page" {
		t.Fatalf("Describe = %q", got)
	}
	long := Web{URL: "https://example.invalid/" + strings.Repeat("a", 100)}
	if got := long.Describe(); len(got) != len("web ")+64 || !strings.HasSuffix(got, "...") {
		t.Fatalf("Describe of a long URL = %q (%d bytes)", got, len(got))
	}
}

func TestSentinelContent(t *testing.T) {
	t.Parallel()
	s := Sentinel{Label: "panel 3"}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if s.kind() != contentSentinel || s.payload() != "panel 3" {
		t.Fatalf("kind/payload = %d/%q", s.kind(), s.payload())
	}
	if got := s.Describe(); got != "sentinel panel 3" {
		t.Fatalf("Describe = %q", got)
	}
	err := Sentinel{Label: strings.Repeat("x", 65)}.Validate()
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("a 65-byte label: %v, want ErrInvalidOption", err)
	}
}

// SentinelColorAt is what the live proof asserts on, so what it refuses to
// answer for matters as much as what it answers.
func TestSentinelColorAt(t *testing.T) {
	t.Parallel()
	const w, h = 640, 480
	for _, tc := range []struct {
		name string
		x, y int
		want uint32
		ok   bool
	}{
		{"top left quadrant", w / 8, h / 8, SentinelTopLeft, true},
		{"bottom right quadrant", w * 7 / 8, h * 7 / 8, SentinelBottomRight, true},
		{"top right quadrant", w * 7 / 8, h / 8, SentinelBackground, true},
		{"bottom left quadrant", w / 8, h * 7 / 8, SentinelBackground, true},
		{"the vertical seam", w / 2, h / 8, 0, false},
		{"the label band", w / 8, h / 2, 0, false},
		{"off the left", -1, 10, 0, false},
		{"off the top", 10, -1, 0, false},
		{"off the right", w, 10, 0, false},
		{"off the bottom", 10, h, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := SentinelColorAt(w, h, tc.x, tc.y)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("SentinelColorAt(%d,%d,%d,%d) = %#06x,%v want %#06x,%v",
					w, h, tc.x, tc.y, got, ok, tc.want, tc.ok)
			}
		})
	}
	for _, d := range []struct{ w, h int }{{0, 4}, {4, 0}, {-1, 4}, {4, -1}} {
		if _, ok := SentinelColorAt(d.w, d.h, 0, 0); ok {
			t.Fatalf("SentinelColorAt accepted a %dx%d display", d.w, d.h)
		}
	}
}

func TestOpenDisplayMessageRoundTrip(t *testing.T) {
	t.Parallel()
	m := OpenDisplayMessage{Width: 1920, Height: 1080, DensityDPI: 320, Slots: 3,
		ContentKind: contentWeb, ContentPayload: "https://example.invalid/"}
	got, err := DecodeOpenDisplay(EncodeOpenDisplay(m))
	if err != nil {
		t.Fatalf("DecodeOpenDisplay: %v", err)
	}
	if got != m {
		t.Fatalf("round trip = %+v, want %+v", got, m)
	}
	if _, err := DecodeOpenDisplay(make([]byte, 19)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("short body: %v, want ErrShortPayload", err)
	}
	// A length prefix that overruns the body: the string decoder must refuse.
	b := EncodeOpenDisplay(m)
	b[20], b[21], b[22], b[23] = 0x7f, 0xff, 0xff, 0xff
	if _, err := DecodeOpenDisplay(b); err == nil {
		t.Fatal("DecodeOpenDisplay accepted an overrunning payload length")
	}
}

func TestDeriveWallSocket(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		env   string
		envOK bool
		home  string
		want  string
	}{
		{"an explicit name wins", "named", true, "/data/user/0/org.example.app/files", "named"},
		{"derived from HOME", "", false, "/data/user/0/org.example.app/files", "org.example.app" + WallSocketSuffix},
		{"the legacy spelling", "", false, "/data/data/org.example.app/files", "org.example.app" + WallSocketSuffix},
		{"an empty env is not a name", "", true, "/data/user/0/org.example.app/files", "org.example.app" + WallSocketSuffix},
		{"nothing to go on", "", false, "/home/nobody", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := DeriveWallSocket(tc.env, tc.envOK, tc.home); got != tc.want {
				t.Fatalf("DeriveWallSocket = %q, want %q", got, tc.want)
			}
		})
	}
	// The two hosts must never be given the same socket name.
	if SocketSuffix == WallSocketSuffix {
		t.Fatal("the capture host and the wall host share a socket suffix")
	}
}
