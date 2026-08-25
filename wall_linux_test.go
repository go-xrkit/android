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

func wallCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestWallAvailable(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })

	lookupEnv = func(string) (string, bool) { return "", false }
	if WallAvailable() {
		t.Fatal("WallAvailable with no environment at all")
	}
	lookupEnv = func(k string) (string, bool) {
		if k == EnvWallSocket {
			return "named-by-the-host", true
		}
		return "", false
	}
	if !WallAvailable() {
		t.Fatal("WallAvailable with the socket named")
	}
	lookupEnv = func(k string) (string, bool) {
		if k == "HOME" {
			return "/data/user/0/org.example.app/files", true
		}
		return "", false
	}
	if !WallAvailable() {
		t.Fatal("WallAvailable with HOME to derive from")
	}
	if got, want := wallSocketName(), "org.example.app"+WallSocketSuffix; got != want {
		t.Fatalf("wallSocketName = %q, want %q", got, want)
	}
}

// The whole path, end to end: a real socket, a real memfd, a real SCM_RIGHTS
// handover, and pixels asserted against the same sentinel the live proof uses.
func TestWallOpenDeliversTheSentinelPixels(t *testing.T) {
	fw := newFakeWall(t, 0)
	w, err := NewWall(4)
	if err != nil {
		t.Fatalf("NewWall: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })

	const dw, dh = 64, 32
	d, err := w.Open(wallCtx(t), DisplaySpec{Width: dw, Height: dh, Content: Sentinel{Label: "p"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if got := d.ID(); got != 2 {
		t.Fatalf("ID = %d, want the host's 2", got)
	}
	if gw, gh := d.Size(); gw != dw || gh != dh {
		t.Fatalf("Size = %dx%d, want %dx%d", gw, gh, dw, dh)
	}
	if d.Format() != FormatRGBA {
		t.Fatalf("Format = %s", d.Format())
	}

	fw.mu.Lock()
	p := fw.panels[0]
	fw.mu.Unlock()
	p.paintSentinel()

	f, err := d.WaitFrame(wallCtx(t))
	if err != nil {
		t.Fatalf("WaitFrame: %v", err)
	}
	if !f.Valid() {
		t.Fatalf("frame is not valid: %+v", f)
	}
	// Assert on the PIXELS, at points SentinelColorAt vouches for. A feed that
	// hands back a black buffer is the silent failure this whole mechanism has
	// to be proved against.
	checked := 0
	for y := 0; y < dh; y++ {
		for x := 0; x < dw; x++ {
			want, ok := SentinelColorAt(dw, dh, x, y)
			if !ok {
				continue
			}
			row := f.Row(y)
			got := uint32(row[x*4])<<16 | uint32(row[x*4+1])<<8 | uint32(row[x*4+2])
			if got != want {
				t.Fatalf("pixel (%d,%d) = %#06x, want %#06x", x, y, got, want)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no pixel was checked; the sampling grid vouched for nothing")
	}
	t.Logf("asserted %d pixels against the sentinel", checked)
}

// Several at once, each with its own connection, its own buffer and its own
// frames — and no panel's pixels reaching another's feed.
func TestWallOpensSeveralIndependentDisplays(t *testing.T) {
	fw := newFakeWall(t, 0)
	w, _ := NewWall(4)
	t.Cleanup(func() { _ = w.Close() })

	ds := make([]*OwnedDisplay, 0, 4)
	for i := 0; i < 4; i++ {
		d, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		ds = append(ds, d)
	}
	ids := map[int]bool{}
	for _, d := range ds {
		if ids[d.ID()] {
			t.Fatalf("two displays share id %d", d.ID())
		}
		ids[d.ID()] = true
	}
	if got := fw.live(); got != 4 {
		t.Fatalf("the host serves %d connections, want 4 — one per display", got)
	}

	// Paint only the third panel; only the third feed must see a frame.
	fw.mu.Lock()
	panels := append([]*fakePanel(nil), fw.panels...)
	fw.mu.Unlock()
	target := panels[2]
	target.paintSentinel()
	if _, err := ds[2].WaitFrame(wallCtx(t)); err != nil {
		t.Fatalf("the painted panel produced no frame: %v", err)
	}
	for i, d := range ds {
		if i == 2 {
			continue
		}
		if _, fresh := d.Frame(); fresh {
			t.Fatalf("display %d saw a frame painted on display 2", i)
		}
	}

	// Closing one leaves the others alone.
	if err := ds[0].Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	target.paintSentinel()
	if _, err := ds[2].WaitFrame(wallCtx(t)); err != nil {
		t.Fatalf("closing another display broke this feed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Wall.Close: %v", err)
	}
}

// The host's own limit, which exists because a second process could open
// displays without using Wall at all.
func TestHostRefusesPastItsOwnLimit(t *testing.T) {
	newFakeWall(t, 2)
	w, _ := NewWall(MaxDisplays) // the WALL allows far more than the host does
	t.Cleanup(func() { _ = w.Close() })

	for i := 0; i < 2; i++ {
		if _, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}}); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
	}
	_, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
	if !errors.Is(err, ErrTooManyDisplays) {
		t.Fatalf("Open past the HOST's limit: %v, want ErrTooManyDisplays", err)
	}
	if !strings.Contains(err.Error(), "at most 2") {
		t.Fatalf("the host's refusal did not reach the caller intact: %v", err)
	}
	if got := w.Len(); got != 2 {
		t.Fatalf("a host refusal left the wall holding %d, want 2", got)
	}
}

func TestWallOpenWithNoHost(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })
	lookupEnv = func(string) (string, bool) { return "", false }

	w, _ := NewWall(1)
	_, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open with no host: %v, want ErrUnsupported", err)
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Len = %d after a hostless Open", got)
	}
}

func TestWallOpenWhenNothingListens(t *testing.T) {
	t.Setenv(EnvWallSocket, "xr-wall-nothing-listens-here")
	oldBudget, oldPause := dialBudget, dialPause
	dialBudget, dialPause = 30*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { dialBudget, dialPause = oldBudget, oldPause })

	w, _ := NewWall(1)
	_, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Open against a dead socket: %v, want ErrUnsupported", err)
	}
}

// Every way the host can answer the handshake wrongly. Each must fail the open
// and leave the wall empty rather than hand back a half-built feed.
func TestWallOpenRefusesAMalformedHandshake(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(*fakePanel, OpenDisplayMessage) bool
		want string
	}{
		{
			name: "a config too short to decode",
			hook: func(p *fakePanel, _ OpenDisplayMessage) bool {
				p.send(MsgConfig, make([]byte, 8))
				return true
			},
			want: "config is 8 bytes",
		},
		{
			name: "a config describing no pixels",
			hook: func(p *fakePanel, m OpenDisplayMessage) bool {
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: 0, Height: 0,
					Format: FormatRGBA, Slots: m.Slots, SlotSize: 1}))
				return true
			},
			want: "0x0 capture",
		},
		{
			name: "a stride narrower than the row",
			hook: func(p *fakePanel, m OpenDisplayMessage) bool {
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
					Stride: 4, Format: FormatRGBA, Slots: m.Slots, SlotSize: 1 << 20}))
				return true
			},
			want: "stride 4",
		},
		{
			name: "a buffer that is not the one announced",
			hook: func(p *fakePanel, m OpenDisplayMessage) bool {
				stride := m.Width * 4
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
					Stride: stride, Format: FormatRGBA, Slots: m.Slots,
					SlotSize: int64(stride) * int64(m.Height), DisplayID: 5}))
				// Lend a DIFFERENT shape from the one just announced.
				p.lendBuffer(m.Slots, 64)
				return true
			},
			want: "having announced",
		},
		{
			name: "a buffer with no descriptor at all",
			hook: func(p *fakePanel, m OpenDisplayMessage) bool {
				stride := m.Width * 4
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
					Stride: stride, Format: FormatRGBA, Slots: m.Slots,
					SlotSize: int64(stride) * int64(m.Height), DisplayID: 5}))
				p.send(MsgBuffer, EncodeBuffer(m.Slots, int64(stride)*int64(m.Height)))
				return true
			},
			want: "with fd -1",
		},
		{
			name: "a buffer message too short to decode",
			hook: func(p *fakePanel, m OpenDisplayMessage) bool {
				stride := m.Width * 4
				p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
					Stride: stride, Format: FormatRGBA, Slots: m.Slots,
					SlotSize: int64(stride) * int64(m.Height), DisplayID: 5}))
				p.send(MsgBuffer, make([]byte, 3))
				return true
			},
			want: "buffer",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fw := newFakeWall(t, 0)
			fw.setOnOpen(tc.hook)
			w, _ := NewWall(2)
			t.Cleanup(func() { _ = w.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := w.Open(ctx, DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
			if err == nil {
				t.Fatal("Open accepted a malformed handshake")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
			if got := w.Len(); got != 0 {
				t.Fatalf("a failed Open left %d displays on the wall", got)
			}
		})
	}
}

// A host that says nothing at all must not hang the caller past its context.
func TestWallOpenHonoursItsContext(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnOpen(func(*fakePanel, OpenDisplayMessage) bool { return true }) // answer nothing
	w, _ := NewWall(1)
	t.Cleanup(func() { _ = w.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := w.Open(ctx, DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Open against a silent host: %v, want DeadlineExceeded", err)
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Len = %d after a timed-out Open", got)
	}
}

// A host that announces the config and then goes away between the two halves
// of the handshake.
func TestWallOpenWhenTheHostVanishesMidHandshake(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setOnOpen(func(p *fakePanel, m OpenDisplayMessage) bool {
		stride := m.Width * 4
		p.send(MsgConfig, EncodeConfig(ConfigMessage{Width: m.Width, Height: m.Height,
			Stride: stride, Format: FormatRGBA, Slots: m.Slots,
			SlotSize: int64(stride) * int64(m.Height), DisplayID: 5}))
		_ = p.conn.Close()
		return true
	})
	w, _ := NewWall(1)
	t.Cleanup(func() { _ = w.Close() })
	if _, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}}); err == nil {
		t.Fatal("Open survived the host vanishing mid-handshake")
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Len = %d", got)
	}
}

// Closing the wall must close the sockets, which is what releases the displays
// on the host side.
func TestWallCloseDropsTheHostConnections(t *testing.T) {
	fw := newFakeWall(t, 0)
	w, _ := NewWall(3)
	for i := 0; i < 3; i++ {
		if _, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for fw.live() > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := fw.live(); got != 0 {
		t.Fatalf("%d host connections outlived Wall.Close", got)
	}
}

func TestHostErrorMapsTooManyDisplays(t *testing.T) {
	t.Parallel()
	err := hostError(ErrorMessage{Code: codeTooManyDisplays, Op: "createVirtualDisplay",
		Detail: "at most 32"})
	if !errors.Is(err, ErrTooManyDisplays) {
		t.Fatalf("hostError = %v, want ErrTooManyDisplays", err)
	}
}

// The descriptor the host lends is the one thing here that becomes a mapping
// over somebody else's memory if it is wrong, so a mapping that fails must fail
// the open rather than hand back a display with no pixels behind it.
func TestWallOpenRefusesAnUnmappableBuffer(t *testing.T) {
	newFakeWall(t, 0)
	old := mmap
	t.Cleanup(func() { mmap = old })
	mmap = func(int, int64, int, int, int) ([]byte, error) { return nil, syscall.ENOMEM }

	w, _ := NewWall(1)
	t.Cleanup(func() { _ = w.Close() })
	_, err := w.Open(wallCtx(t), DisplaySpec{Width: 32, Height: 16, Content: Sentinel{}})
	if !errors.Is(err, syscall.ENOMEM) {
		t.Fatalf("Open with an unmappable buffer: %v, want ENOMEM", err)
	}
	if got := w.Len(); got != 0 {
		t.Fatalf("Len = %d after an unmappable buffer", got)
	}
}
