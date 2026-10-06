// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// testBudget is how long a helper-provided context lives.
//
// Ten seconds is generous natively and is NOT enough under qemu-user: on the
// riscv64 lane CaptureDisplay ran out of context at 10.01 s, because the wall
// clock a test waits on is not the clock the emulated code runs on. The
// emulated CI lanes set XRKIT_TEST_BUDGET, so the allowance lives with the
// environment that needs it instead of every test being loosened on every
// architecture -- a budget raised everywhere would stop catching a real hang
// on the machines where 10 s is the right answer.
func testBudget() time.Duration {
	if s := os.Getenv("XRKIT_TEST_BUDGET"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Second
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testBudget())
	t.Cleanup(cancel)
	return ctx
}

// start does the whole handshake and hands back a running stream.
func start(t *testing.T, f *fake, o Options) *Stream {
	t.Helper()
	d, err := DefaultDisplay(ctxT(t))
	if err != nil {
		t.Fatalf("DefaultDisplay: %v", err)
	}
	st, err := CaptureDisplay(ctxT(t), d, o)
	if err != nil {
		t.Fatalf("CaptureDisplay: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.waitBuffer()
	return st
}

func TestAvailableFollowsTheEnvironment(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old })

	lookupEnv = func(string) (string, bool) { return "", false }
	if Available() {
		t.Error("Available with nothing set at all")
	}
	lookupEnv = func(k string) (string, bool) {
		if k == EnvSocket {
			return "something", true
		}
		return "", false
	}
	if !Available() {
		t.Error("Available said no with a socket name set")
	}
	// And the composed case: no socket named, but a HOME that says which
	// package this is.
	lookupEnv = func(k string) (string, bool) {
		if k == "HOME" {
			return "/data/user/0/org.example.app/files", true
		}
		return "", false
	}
	if !Available() {
		t.Error("Available said no with a derivable package name")
	}
}

func TestWithoutAHostEverythingReportsUnsupported(t *testing.T) {
	old := lookupEnv
	t.Cleanup(func() { lookupEnv = old; resetForTest() })
	lookupEnv = func(string) (string, bool) { return "", false }
	resetForTest()

	if _, err := Displays(ctxT(t)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Displays = %v, want ErrUnsupported", err)
	}
	if _, err := DefaultDisplay(ctxT(t)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("DefaultDisplay = %v, want ErrUnsupported", err)
	}
	if Authorized() {
		t.Error("Authorized with no host")
	}
	if _, err := RequestAuthorization(ctxT(t)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("RequestAuthorization = %v, want ErrUnsupported", err)
	}
	if _, err := CaptureDisplay(ctxT(t), Display{}, Options{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("CaptureDisplay = %v, want ErrUnsupported", err)
	}
}

// TestTheHostIsWaitedForWhileItComesUp: nothing orders the host's listener
// against the application's first call, so a refusal early on means "not yet".
func TestTheHostIsWaitedForWhileItComesUp(t *testing.T) {
	name := fmt.Sprintf("xr-late-%d", os.Getpid())
	t.Setenv(EnvSocket, name)
	resetForTest()
	t.Cleanup(resetForTest)
	go func() {
		time.Sleep(120 * time.Millisecond)
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
		if err != nil {
			return
		}
		defer ln.Close()
		c, err := ln.AcceptUnix()
		if err != nil {
			return
		}
		defer c.Close()
		if _, _, err := ReadMessage(c); err != nil {
			return
		}
		_, _ = c.Write(FrameMessage(MsgDisplays, EncodeDisplays([]Display{{ID: 0, Name: "late"}})))
		time.Sleep(200 * time.Millisecond)
	}()
	ds, err := Displays(ctxT(t))
	if err != nil {
		t.Fatalf("Displays against a host that was still coming up: %v", err)
	}
	if ds[0].Name != "late" {
		t.Errorf("got %v", ds)
	}
}

func TestDialFailureIsReportedAndRemembered(t *testing.T) {
	old := dialBudget
	t.Cleanup(func() { dialBudget = old })
	dialBudget = 0 // one attempt, no waiting: the socket is known to be absent
	t.Setenv(EnvSocket, "xr-nothing-listens-here")
	resetForTest()
	t.Cleanup(resetForTest)
	_, err := Displays(ctxT(t))
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Displays = %v, want ErrUnsupported", err)
	}
	// The second call must not re-dial: the host is either there for the life
	// of the process or it is not.
	oldDial := dialUnix
	t.Cleanup(func() { dialUnix = oldDial })
	dialUnix = func(string, *net.UnixAddr, *net.UnixAddr) (*net.UnixConn, error) {
		t.Error("a failed dial was retried")
		return nil, errors.New("no")
	}
	if _, err := Displays(ctxT(t)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Displays = %v, want ErrUnsupported", err)
	}
}

func TestDisplaysCrossTheWire(t *testing.T) {
	f := newFake(t)
	ds, err := Displays(ctxT(t))
	if err != nil {
		t.Fatalf("Displays: %v", err)
	}
	if len(ds) != len(f.displays) {
		t.Fatalf("got %d displays, want %d", len(ds), len(f.displays))
	}
	if ds[0].Name != "Built-in Screen" || !ds[0].Default() {
		t.Errorf("display 0 = %v", ds[0])
	}
	// The glasses are the reason the list is exposed at all: they are not
	// capturable, but they ARE where the application draws.
	if ds[1].Name != "VITURE Beast" || !ds[1].Presentation() || ds[1].Default() {
		t.Errorf("display 1 = %v", ds[1])
	}
	if got := ds[0].RefreshRate; got < 60 || got > 60.001 {
		t.Errorf("refresh rate %v did not survive the milli-hertz encoding", got)
	}
}

func TestDisplaysReportsAnEmptyList(t *testing.T) {
	f := newFake(t)
	f.setDisplays(nil)
	if _, err := Displays(ctxT(t)); !errors.Is(err, ErrNoDisplay) {
		t.Errorf("Displays = %v, want ErrNoDisplay", err)
	}
}

func TestDisplaysRejectsAMalformedList(t *testing.T) {
	f := newFake(t)
	f.setOnList(func(f *fake) { f.send(MsgDisplays, []byte{0, 0}) })
	if _, err := Displays(ctxT(t)); !errors.Is(err, ErrShortPayload) {
		t.Errorf("Displays = %v, want ErrShortPayload", err)
	}
}

func TestDefaultDisplayNeedsOne(t *testing.T) {
	f := newFake(t)
	f.setDisplays([]Display{{ID: 7, Name: "Overlay #1", Width: 4, Height: 2}})
	if _, err := DefaultDisplay(ctxT(t)); !errors.Is(err, ErrNotFound) {
		t.Errorf("DefaultDisplay = %v, want ErrNotFound", err)
	}
}

func TestConsent(t *testing.T) {
	f := newFake(t)
	if !Authorized() {
		t.Error("Authorized said no when the host holds a token")
	}
	ok, err := RequestAuthorization(ctxT(t))
	if err != nil || !ok {
		t.Errorf("RequestAuthorization = %v, %v", ok, err)
	}

	// A refusal is a false, not an error: the user said no, nothing broke.
	f.setConsent(false)
	if Authorized() {
		t.Error("Authorized said yes with no token")
	}
	ok, err = RequestAuthorization(ctxT(t))
	if err != nil || ok {
		t.Errorf("RequestAuthorization after a refusal = %v, %v", ok, err)
	}

	// A host that answers a consent request with rubbish.
	f.setOnConsent(func(f *fake, _ bool) { f.send(MsgConsent, nil) })
	if Authorized() {
		t.Error("Authorized believed an empty answer")
	}
	if _, err := RequestAuthorization(ctxT(t)); !errors.Is(err, ErrShortPayload) {
		t.Errorf("RequestAuthorization = %v, want ErrShortPayload", err)
	}
}

func TestConsentDenialFromTheHostIsTheSentinel(t *testing.T) {
	f := newFake(t)
	f.setOnConsent(func(f *fake, _ bool) {
		f.send(MsgError, EncodeError(ErrorMessage{Code: codeConsentDenied,
			Op: "getMediaProjection", Detail: "the user declined"}))
	})
	if _, err := RequestAuthorization(ctxT(t)); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("RequestAuthorization = %v, want ErrPermissionDenied", err)
	}
	if Authorized() {
		t.Error("Authorized believed a denial")
	}
}

func TestHostErrorsMapOntoSentinels(t *testing.T) {
	for _, tc := range []struct {
		code int
		want error
	}{
		{codeConsentDenied, ErrPermissionDenied},
		{codeNoDisplay, ErrNoDisplay},
		{codeNotFound, ErrNotFound},
		{codeNotCapturable, ErrNotCapturable},
	} {
		got := hostError(ErrorMessage{Code: tc.code, Op: "op", Detail: "why"})
		if !errors.Is(got, tc.want) {
			t.Errorf("code %d mapped to %v, want %v", tc.code, got, tc.want)
		}
	}
	// A code with no sentinel keeps the host's own wording.
	got := hostError(ErrorMessage{Code: 99, Op: "createVirtualDisplay", Detail: "boom"})
	if !strings.Contains(got.Error(), "createVirtualDisplay") || !strings.Contains(got.Error(), "boom") {
		t.Errorf("unmapped code lost its message: %v", got)
	}
	if errors.Is(got, ErrPermissionDenied) {
		t.Error("an unmapped code matched a sentinel")
	}
}

func TestHostErrorWithARuinedBodyIsADecodeError(t *testing.T) {
	f := newFake(t)
	f.setOnList(func(f *fake) { f.send(MsgError, []byte{0, 0}) })
	if _, err := Displays(ctxT(t)); !errors.Is(err, ErrShortPayload) {
		t.Errorf("Displays = %v, want ErrShortPayload", err)
	}
}

func TestARequestGivesUpWithItsContext(t *testing.T) {
	f := newFake(t)
	f.setOnList(func(*fake) {}) // answer nothing at all
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := Displays(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Displays = %v, want DeadlineExceeded", err)
	}
	// The abandoned answer must not be handed to the NEXT request.
	f.send(MsgDisplays, EncodeDisplays([]Display{{ID: 99, Name: "stale"}}))
	time.Sleep(30 * time.Millisecond)
	f.setOnList(nil)
	ds, err := Displays(ctxT(t))
	if err != nil {
		t.Fatalf("Displays: %v", err)
	}
	if ds[0].ID == 99 {
		t.Error("the next request was handed the abandoned answer")
	}
}

func TestAStaleReplyOfTheWrongTypeIsIgnored(t *testing.T) {
	f := newFake(t)
	f.setOnList(func(f *fake) {
		f.send(MsgConsent, EncodeConsent(true)) // an answer to nothing
		f.send(MsgDisplays, EncodeDisplays(f.displays))
	})
	ds, err := Displays(ctxT(t))
	if err != nil {
		t.Fatalf("Displays: %v", err)
	}
	if len(ds) != 2 {
		t.Errorf("got %d displays through a stale reply", len(ds))
	}
}

func TestAVanishedHostFailsEveryRequest(t *testing.T) {
	f := newFake(t)
	f.setOnList(func(f *fake) { f.closeConn() })
	if _, err := Displays(ctxT(t)); err == nil {
		t.Fatal("a request survived the host going away")
	}
	if _, err := Displays(ctxT(t)); err == nil {
		t.Fatal("a request after the host went away succeeded")
	}
}

func TestCaptureRefusesADisplayItMayNotCapture(t *testing.T) {
	newFake(t)
	_, err := CaptureDisplay(ctxT(t), Display{ID: 3, Name: "VITURE Beast", Width: 8, Height: 4}, Options{})
	if !errors.Is(err, ErrNotCapturable) {
		t.Fatalf("CaptureDisplay on a second display = %v, want ErrNotCapturable", err)
	}
	if !strings.Contains(err.Error(), "CAPTURE_VIDEO_OUTPUT") {
		t.Errorf("the error does not name the permission that would be needed: %v", err)
	}
}

func TestCaptureValidatesItsOptions(t *testing.T) {
	newFake(t)
	d, err := DefaultDisplay(ctxT(t))
	if err != nil {
		t.Fatalf("DefaultDisplay: %v", err)
	}
	if _, err := CaptureDisplay(ctxT(t), d, Options{QueueDepth: 2}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay = %v, want ErrInvalidOption", err)
	}
	if _, err := CaptureDisplay(ctxT(t), Display{ID: 0, Name: "no size"}, Options{}); !errors.Is(err, ErrInvalidOption) {
		t.Errorf("CaptureDisplay on a sizeless display = %v, want ErrInvalidOption", err)
	}
}

func TestOnlyOneCaptureAtATime(t *testing.T) {
	f := newFake(t)
	start(t, f, Options{})
	d, _ := DefaultDisplay(ctxT(t))
	if _, err := CaptureDisplay(ctxT(t), d, Options{}); err == nil {
		t.Fatal("a second capture started while one was running")
	}
}

func TestCaptureSurfacesAHostRefusal(t *testing.T) {
	f := newFake(t)
	f.setOnStart(func(f *fake, _ StartMessage) {
		f.send(MsgError, EncodeError(ErrorMessage{Code: codeNotCapturable,
			Op: "createVirtualDisplay", Detail: "no token"}))
	})
	d, _ := DefaultDisplay(ctxT(t))
	if _, err := CaptureDisplay(ctxT(t), d, Options{}); !errors.Is(err, ErrNotCapturable) {
		t.Fatalf("CaptureDisplay = %v, want ErrNotCapturable", err)
	}
}

func TestCaptureRejectsARuinedConfig(t *testing.T) {
	f := newFake(t)
	f.setOnStart(func(f *fake, _ StartMessage) { f.send(MsgConfig, []byte{1, 2, 3}) })
	d, _ := DefaultDisplay(ctxT(t))
	if _, err := CaptureDisplay(ctxT(t), d, Options{}); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("CaptureDisplay = %v, want ErrShortPayload", err)
	}
}

func TestCaptureRejectsAnImpossibleConfig(t *testing.T) {
	good := ConfigMessage{Width: 8, Height: 4, Stride: 40, Format: FormatRGBA, Slots: 3, SlotSize: 160}
	for name, bad := range map[string]ConfigMessage{
		"no size":       {Width: 0, Height: 4, Stride: 40, Format: FormatRGBA, Slots: 3, SlotSize: 160},
		"absurd size":   {Width: MaxDimension + 1, Height: 4, Stride: 1 << 20, Format: FormatRGBA, Slots: 3, SlotSize: 1 << 30},
		"short stride":  {Width: 8, Height: 4, Stride: 8, Format: FormatRGBA, Slots: 3, SlotSize: 160},
		"wrong format":  {Width: 8, Height: 4, Stride: 40, Format: 99, Slots: 3, SlotSize: 160},
		"too few slots": {Width: 8, Height: 4, Stride: 40, Format: FormatRGBA, Slots: 1, SlotSize: 160},
		"too many":      {Width: 8, Height: 4, Stride: 40, Format: FormatRGBA, Slots: 99, SlotSize: 160},
		"small slot":    {Width: 8, Height: 4, Stride: 40, Format: FormatRGBA, Slots: 3, SlotSize: 8},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.setConfig(bad)
			d, _ := DefaultDisplay(ctxT(t))
			if _, err := CaptureDisplay(ctxT(t), d, Options{}); err == nil {
				t.Fatalf("CaptureDisplay accepted %+v", bad)
			}
			<-f.stopped // a refused config must not leave the host capturing
		})
	}
	if err := checkConfig(good); err != nil {
		t.Errorf("checkConfig rejected a good config: %v", err)
	}
}

func TestMappingTheLentBufferCanFail(t *testing.T) {
	f := newFake(t)
	old := mmap
	t.Cleanup(func() { mmap = old })
	mmap = func(int, int64, int, int, int) ([]byte, error) { return nil, syscall.ENOMEM }
	d, _ := DefaultDisplay(ctxT(t))
	if _, err := CaptureDisplay(ctxT(t), d, Options{}); err == nil {
		t.Fatal("CaptureDisplay succeeded with an unmappable frame buffer")
	}
	<-f.stopped
}

// TestCaptureRefusesABufferThatDoesNotMatchTheConfig: the descriptor is the one
// thing here that would become a wrong-sized mapping over somebody else's
// memory, so what the host lends is checked against what it announced.
func TestCaptureRefusesABufferThatDoesNotMatchTheConfig(t *testing.T) {
	for name, lend := range map[string]func(f *fake){
		"a ruined body":   func(f *fake) { f.send(MsgBuffer, []byte{1, 2}) },
		"no descriptor":   func(f *fake) { f.send(MsgBuffer, EncodeBuffer(3, 160)) },
		"the wrong shape": func(f *fake) { f.lendBuffer(4, 160) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			f.setOnStart(func(f *fake, _ StartMessage) {
				f.send(MsgConfig, EncodeConfig(f.config()))
				lend(f)
			})
			d, _ := DefaultDisplay(ctxT(t))
			if _, err := CaptureDisplay(ctxT(t), d, Options{}); err == nil {
				t.Fatal("CaptureDisplay accepted a buffer that did not match the config")
			}
			<-f.stopped
		})
	}
}

// TestCaptureGivesUpWaitingForTheBuffer: a host that configures a capture and
// then never lends anything must not hang the caller past its context.
func TestCaptureGivesUpWaitingForTheBuffer(t *testing.T) {
	f := newFake(t)
	f.setOnStart(func(f *fake, _ StartMessage) { f.send(MsgConfig, EncodeConfig(f.config())) })
	d, _ := DefaultDisplay(ctxT(t))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, err := CaptureDisplay(ctx, d, Options{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CaptureDisplay = %v, want DeadlineExceeded", err)
	}
}

func TestHandingOverTheBufferCanFail(t *testing.T) {
	f := newFake(t)
	f.setOnStart(func(f *fake, _ StartMessage) {
		f.send(MsgConfig, EncodeConfig(f.config()))
		f.closeConn() // the host dies between the config and the buffer
	})
	d, _ := DefaultDisplay(ctxT(t))
	// Either the send fails or the pump notices first; both are failures, and
	// neither may leave a stream behind.
	if st, err := CaptureDisplay(ctxT(t), d, Options{}); err == nil {
		_ = st.Close()
		t.Skip("the host's death was not observed before the handover; not a defect")
	}
}

func TestAFrameArrivesAndItsContentChanges(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})

	if _, fresh := st.Frame(); fresh {
		t.Error("a frame was fresh before any arrived")
	}
	f.paint(0x11)
	got, err := st.WaitFrame(ctxT(t))
	if err != nil {
		t.Fatalf("WaitFrame: %v", err)
	}
	if got.Width != 8 || got.Height != 4 || got.Stride != 40 {
		t.Fatalf("frame geometry %dx%d stride %d", got.Width, got.Height, got.Stride)
	}
	if got.Pix[0] != 0x11 {
		t.Fatalf("frame content %#x, want 0x11", got.Pix[0])
	}
	if got.Seq != 1 {
		t.Errorf("Seq = %d, want 1", got.Seq)
	}
	if got.At.IsZero() {
		t.Error("the frame carried no timestamp")
	}

	// A SECOND frame must change what the borrow shows. A buffer that never
	// changes is the classic silent failure of a capture.
	f.paint(0x22)
	got2, err := st.WaitFrame(ctxT(t))
	if err != nil {
		t.Fatalf("WaitFrame: %v", err)
	}
	if got2.Pix[0] != 0x22 {
		t.Fatalf("second frame content %#x, want 0x22 — the buffer is frozen", got2.Pix[0])
	}
	if got2.Seq != 2 {
		t.Errorf("Seq = %d, want 2", got2.Seq)
	}

	// And the frames really are in different slots, so a borrow held across a
	// delivery is not overwritten under the consumer.
	f.paint(0x33)
	if err := waitSeq(st, 3); err != nil {
		t.Fatal(err)
	}
	if got2.Pix[0] != 0x22 {
		t.Error("a live borrow was overwritten by the next frame")
	}
}

func waitSeq(st *Stream, seq uint64) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st.Stats().Frames >= seq {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("timed out waiting for frames")
}

func TestFrameReportsFreshness(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.paint(0x44)
	if err := waitSeq(st, 1); err != nil {
		t.Fatal(err)
	}
	if _, fresh := st.Frame(); !fresh {
		t.Error("the first frame was not fresh")
	}
	got, fresh := st.Frame()
	if fresh {
		t.Error("the same frame was fresh twice")
	}
	if got.Seq != 1 || got.Pix[0] != 0x44 {
		t.Errorf("a stale read lost the pixels: %v", got.Seq)
	}
}

func TestSupersededFramesAreCounted(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.paint(1)
	f.paint(2)
	f.paint(3)
	if err := waitSeq(st, 3); err != nil {
		t.Fatal(err)
	}
	if got := st.Stats().Superseded; got < 2 {
		t.Errorf("Superseded = %d, want at least 2", got)
	}
	if st.Stats().Interval <= 0 {
		t.Error("no interval between frames was measured")
	}
	if st.Stats().FPS() <= 0 {
		t.Error("FPS() = 0 with several frames delivered")
	}
	f2, fresh := st.Frame()
	if !fresh || f2.Seq != 3 {
		t.Errorf("Frame after three deliveries = seq %d fresh %v, want the newest", f2.Seq, fresh)
	}
}

func TestAFrameOutsideTheMappingIsRefused(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	for _, bad := range []FrameMsg{
		{Seq: 1, Slot: -1, Width: 8, Height: 4, Stride: 40},
		{Seq: 1, Slot: 99, Width: 8, Height: 4, Stride: 40},
		{Seq: 1, Slot: 0, Width: 0, Height: 4, Stride: 40},
		{Seq: 1, Slot: 0, Width: 8, Height: 4, Stride: 4},
		{Seq: 1, Slot: 0, Width: 8, Height: 4000, Stride: 40},
	} {
		f.send(MsgFrame, EncodeFrame(bad))
	}
	time.Sleep(80 * time.Millisecond)
	if n := st.Stats().Frames; n != 0 {
		t.Fatalf("%d impossible frames were accepted", n)
	}
	if _, fresh := st.Frame(); fresh {
		t.Error("an impossible frame became visible")
	}
}

func TestARuinedFrameMessageEndsTheStream(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.send(MsgFrame, []byte{1, 2, 3})
	if _, err := st.WaitFrame(ctxT(t)); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("WaitFrame = %v, want ErrShortPayload", err)
	}
	if !errors.Is(st.Err(), ErrShortPayload) {
		t.Errorf("Err = %v, want ErrShortPayload", st.Err())
	}
}

func TestTheUserStoppingTheCaptureIsPermissionDenied(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.send(MsgStopped, EncodeStopped(StoppedMessage{Reason: StopUser, Detail: "cast chip"}))
	if _, err := st.WaitFrame(ctxT(t)); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("WaitFrame = %v, want ErrPermissionDenied", err)
	}
	if !strings.Contains(st.Err().Error(), "cast chip") {
		t.Errorf("the host's own wording was lost: %v", st.Err())
	}
}

func TestTheSystemStoppingTheCaptureIsAPlainError(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.send(MsgStopped, EncodeStopped(StoppedMessage{Reason: StopSystem, Detail: "screen off"}))
	err := waitErr(t, st)
	if errors.Is(err, ErrPermissionDenied) {
		t.Errorf("a system stop was reported as a denial: %v", err)
	}
	if !strings.Contains(err.Error(), "screen off") {
		t.Errorf("Err = %v", err)
	}
}

func TestARuinedStoppedMessageEndsTheStream(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.send(MsgStopped, nil)
	if err := waitErr(t, st); !errors.Is(err, ErrShortPayload) {
		t.Fatalf("Err = %v, want ErrShortPayload", err)
	}
}

func waitErr(t *testing.T, st *Stream) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := st.Err(); err != nil {
			return err
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the stream never reported an error")
	return nil
}

func TestTheHostGoingAwayEndsTheStream(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.closeConn()
	if err := waitErr(t, st); err == nil {
		t.Fatal("the stream survived the host")
	}
	if _, err := st.WaitFrame(ctxT(t)); err == nil {
		t.Error("WaitFrame succeeded after the host went away")
	}
}

func TestWaitFrameGivesUpWithItsContext(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := st.WaitFrame(ctx); !errors.Is(err, ErrNoFrame) {
		t.Fatalf("WaitFrame = %v, want ErrNoFrame", err)
	}
}

func TestCloseIsIdempotentAndEndsEverything(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.paint(9)
	if err := waitSeq(st, 1); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	<-f.stopped
	if _, fresh := st.Frame(); fresh {
		t.Error("Frame was fresh after Close")
	}
	if _, err := st.WaitFrame(ctxT(t)); !errors.Is(err, ErrClosed) {
		t.Errorf("WaitFrame after Close = %v, want ErrClosed", err)
	}
	// A frame arriving after Close must be dropped, not written into a
	// mapping that is gone.
	f.send(MsgFrame, EncodeFrame(FrameMsg{Seq: 5, Slot: 0, Width: 8, Height: 4, Stride: 40}))
	time.Sleep(40 * time.Millisecond)

	// And a new capture may start once the old one is closed.
	d, _ := DefaultDisplay(ctxT(t))
	st2, err := CaptureDisplay(ctxT(t), d, Options{})
	if err != nil {
		t.Fatalf("CaptureDisplay after Close: %v", err)
	}
	_ = st2.Close()
}

func TestCloseReportsAnUnmapFailure(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	mapped := st.buf
	old := munmap
	t.Cleanup(func() { munmap = old })
	munmap = func([]byte) error { return syscall.EINVAL }
	if err := st.Close(); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("Close = %v, want EINVAL", err)
	}
	// Close reported the failure and dropped its reference, so the mapping is
	// this test's to release.
	if err := old(mapped); err != nil {
		t.Errorf("releasing the mapping by hand: %v", err)
	}
}

func TestStreamDescribesItself(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{FPS: 30, QueueDepth: 4})
	if w, h := st.Size(); w != 8 || h != 4 {
		t.Errorf("Size = %dx%d", w, h)
	}
	if st.Format() != FormatRGBA {
		t.Errorf("Format = %v", st.Format())
	}
	if got := st.Options(); got.FPS != 30 || got.QueueDepth != 4 || got.Width != 8 {
		t.Errorf("Options = %+v", got)
	}
	if !strings.Contains(st.String(), "8x4") || !strings.Contains(st.String(), "RGBA_8888") {
		t.Errorf("String = %q", st.String())
	}
	if st.Err() != nil {
		t.Errorf("Err on a healthy stream = %v", st.Err())
	}
}

// TestFrameIsBorrowedNotCopied is the API's central promise, and the only way
// to state it is to write through the host's mapping and see it in a frame the
// consumer already holds.
func TestFrameIsBorrowedNotCopied(t *testing.T) {
	f := newFake(t)
	st := start(t, f, Options{})
	f.paint(0x55)
	got, err := st.WaitFrame(ctxT(t))
	if err != nil {
		t.Fatalf("WaitFrame: %v", err)
	}
	f.mu.Lock()
	f.buf[int64(got.Seq-1)%int64(f.slots)*f.slotSz] = 0x66
	f.mu.Unlock()
	if got.Pix[0] != 0x66 {
		t.Fatal("the frame was a copy: the API's whole no-allocation promise rests on it not being one")
	}
}

func BenchmarkFrame(b *testing.B) {
	f := newFake(b)
	d, err := DefaultDisplay(context.Background())
	if err != nil {
		b.Fatalf("DefaultDisplay: %v", err)
	}
	st, err := CaptureDisplay(context.Background(), d, Options{})
	if err != nil {
		b.Fatalf("CaptureDisplay: %v", err)
	}
	defer st.Close()
	f.waitBuffer()
	f.paint(1)
	for range 200 {
		if st.Stats().Frames > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if fr, _ := st.Frame(); len(fr.Pix) == 0 {
			b.Fatal("no pixels")
		}
	}
}

// TestStrayDescriptorsAreNotLeaked. A descriptor that arrives where none
// belongs is the one thing here that leaks silently and then exhausts the
// process, so this counts the process's OWN open descriptors rather than
// trusting a call site to have closed them.
func TestStrayDescriptorsAreNotLeaked(t *testing.T) {
	f := newFake(t)
	if _, err := Displays(ctxT(t)); err != nil {
		t.Fatalf("Displays: %v", err)
	}
	before := openFDs(t)

	for range 20 {
		// A descriptor on a message that never carries one; a buffer nobody
		// asked for; and a second one while the reply slot is still full.
		// The buffer goes first so the one left waiting in the reply slot is
		// the one CARRYING a descriptor: that is the reply a later request has
		// to drain and close.
		f.sendWithFD(MsgBuffer, EncodeBuffer(3, 160))
		f.sendWithFD(MsgConsent, EncodeConsent(true))
		f.sendWithFD(MsgBuffer, EncodeBuffer(3, 160))
	}
	time.Sleep(150 * time.Millisecond)
	// And a stale buffer arriving in front of the answer a request waits for.
	f.setOnList(func(f *fake) {
		f.sendWithFD(MsgBuffer, EncodeBuffer(3, 160))
		time.Sleep(10 * time.Millisecond)
		f.send(MsgDisplays, EncodeDisplays(f.displays))
	})
	for range 20 {
		if _, err := Displays(ctxT(t)); err != nil {
			t.Fatalf("Displays: %v", err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	after := openFDs(t)
	// One descriptor may legitimately still be held, unclaimed, waiting for a
	// message that will never come. Eighty may not.
	if after > before+2 {
		t.Errorf("open descriptors went from %d to %d over 80 stray ones: they are leaking",
			before, after)
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	names, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	return len(names)
}

// TestTakeRightsIgnoresRubbish: ancillary data comes from outside the process,
// so it is parsed defensively rather than trusted.
func TestTakeRightsIgnoresRubbish(t *testing.T) {
	c := newFDConn(nil)
	// A control message header claiming far more bytes than the buffer holds.
	var bad unix.Cmsghdr
	bad.Level = unix.SOL_SOCKET
	bad.Type = unix.SCM_RIGHTS
	bad.SetLen(1 << 20)
	badOOB := make([]byte, unsafe.Sizeof(bad))
	copy(badOOB, (*(*[unsafe.Sizeof(bad)]byte)(unsafe.Pointer(&bad)))[:])
	c.takeRights(badOOB)
	if c.fd != -1 {
		t.Errorf("an impossible control message produced descriptor %d", c.fd)
	}
	// A well-formed control message that is not SCM_RIGHTS.
	var h unix.Cmsghdr
	h.Level = unix.SOL_SOCKET
	h.Type = unix.SCM_CREDENTIALS
	h.SetLen(unix.CmsgLen(0))
	oob := make([]byte, unix.CmsgSpace(0))
	copy(oob, (*(*[unsafe.Sizeof(h)]byte)(unsafe.Pointer(&h)))[:])
	c.takeRights(oob)
	if c.fd != -1 {
		t.Errorf("a non-SCM_RIGHTS control message produced descriptor %d", c.fd)
	}
}

// TestAnEmptyReadIsEndOfStream: a stream socket signals the peer going away
// with no bytes, no descriptors and no error, and treating that as anything
// else would spin.
func TestAnEmptyReadIsEndOfStream(t *testing.T) {
	c := newFDConn(nil)
	c.read = func([]byte, []byte) (int, int, error) { return 0, 0, nil }
	if _, err := c.Read(make([]byte, 8)); !errors.Is(err, io.EOF) {
		t.Errorf("Read of an empty message = %v, want io.EOF", err)
	}
}

// ⛔⛔ A HANDSHAKE IS TWO MESSAGES, AND THE PUMP MUST HOLD BOTH.
//
// The host answers MsgStart — and MsgOpenDisplay, and MsgOpenScreen — with
// MsgConfig and then MsgBuffer, sent back to back. The pump holds what the
// caller has not consumed yet, and with room for ONE it dropped the buffer
// whenever it read both before the caller woke: the caller then waited out its
// whole context for a descriptor that was already closed, with nothing logged.
//
// It showed up as TestCaptureRefusesABufferThatDoesNotMatchTheConfig and
// TestWallOpensSeveralIndependentDisplays failing about once in thirteen runs
// on one CPU, which reads as flakiness. It is not: a real host sends the same
// two messages back to back, so a loaded phone reaches it the same way, and the
// symptom there is a capture that never starts.
//
// This drives the pump with nobody waiting, which is the condition, rather than
// racing a goroutine against a sleep and hoping.
func TestThePumpHoldsBothHalvesOfAHandshake(t *testing.T) {
	f := newFake(t)
	s, err := connect()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// ⛔ ONE REAL ROUND TRIP FIRST. The fake only writes once it has ACCEPTED
	// the connection, and its send is a silent no-op before that -- so sending
	// the two messages straight after connect would measure the accept race and
	// pass for the wrong reason, holding 0 of 2 with the defect absent.
	if _, err := Displays(ctxT(t)); err != nil {
		t.Fatalf("Displays: %v", err)
	}
	cfg := f.config()
	f.send(MsgConfig, EncodeConfig(cfg))
	f.lendBuffer(cfg.Slots, cfg.SlotSize)

	deadline := time.Now().Add(testBudget())
	for len(s.replies) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the pump is holding %d of the handshake's 2 messages; the second "+
				"was dropped and whoever asked for it would wait forever", len(s.replies))
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := s.await(ctxT(t), MsgConfig); err != nil {
		t.Fatalf("awaiting the config: %v", err)
	}
	r, err := s.await(ctxT(t), MsgBuffer)
	if err != nil {
		t.Fatalf("awaiting the buffer: %v", err)
	}
	if r.fd < 0 {
		t.Fatal("the buffer arrived without its descriptor")
	}
	if err := closeFD(r.fd); err != nil {
		t.Fatalf("closing the lent descriptor: %v", err)
	}
}

// And a request must not be handed what an abandoned one left behind — two
// messages now, not one, so the drain is a loop.
func TestARequestDrainsAWholeAbandonedAnswer(t *testing.T) {
	f := newFake(t)
	s, err := connect()
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// ⛔ ONE REAL ROUND TRIP FIRST. The fake only writes once it has ACCEPTED
	// the connection, and its send is a silent no-op before that -- so sending
	// the two messages straight after connect would measure the accept race and
	// pass for the wrong reason, holding 0 of 2 with the defect absent.
	if _, err := Displays(ctxT(t)); err != nil {
		t.Fatalf("Displays: %v", err)
	}
	cfg := f.config()
	f.send(MsgConfig, EncodeConfig(cfg))
	f.lendBuffer(cfg.Slots, cfg.SlotSize)
	deadline := time.Now().Add(testBudget())
	for len(s.replies) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the pump never held both halves")
		}
		time.Sleep(time.Millisecond)
	}
	// A fresh request must get the DISPLAY LIST it asked for, not the config
	// left over from the handshake nobody collected.
	body, err := s.request(ctxT(t), MsgListDisplays, nil, MsgDisplays)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	ds, err := DecodeDisplays(body)
	if err != nil {
		t.Fatalf("decoding the display list: %v", err)
	}
	if len(ds) == 0 {
		t.Fatal("the display list came back empty")
	}
}
