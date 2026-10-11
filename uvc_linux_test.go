// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// fakeCamera answers control transfers the way a UVC device does, so the whole
// probe/commit sequence runs against something that can disagree.
//
// ⭐ IT CAN CHANGE THE TERMS, WHICH THE REAL DEVICE DID NOT. The Beast's camera
// accepted exactly what it was asked for, so the branch that notices a camera
// answering with another frame or interval would never have run — and that
// branch is the one protecting a caller from believing its own request.
type fakeCamera struct {
	t *testing.T

	probeLen int
	// block is the camera's own probe block, which a host must hand back with
	// the fields it does not understand untouched.
	block []byte
	// answerFrame and answerInterval, when non-zero, are what the camera
	// insists on regardless of what it was asked.
	answerFrame    int
	answerInterval uint32

	// failAt makes one request fail, named by its UVC request code and
	// selector, so each step's error path is reachable.
	failAt func(request uint8, selector uint8) error

	// seen records the sequence, which is what the specification pins.
	seen []string
	// committed is the block the host finally committed.
	committed []byte
}

func newFakeCamera(t *testing.T) *fakeCamera {
	c := &fakeCamera{t: t, probeLen: 34, block: make([]byte, 34)}
	// Fill the fields a host must not rewrite with something recognisable.
	for i := range c.block {
		c.block[i] = 0x5A
	}
	return c
}

// install replaces the control seam with this camera, which answers on a real
// slice rather than through a uintptr.
func (c *fakeCamera) install(t *testing.T) {
	old := controlTransfer
	t.Cleanup(func() { controlTransfer = old })
	controlTransfer = func(_ *USBHandle, requestType, request uint8, value, index uint16, data []byte) (int, error) {
		selector := uint8(value >> 8)
		c.seen = append(c.seen, name(request, selector))
		if c.failAt != nil {
			if err := c.failAt(request, selector); err != nil {
				return 0, err
			}
		}
		switch {
		case request == uvcGetLen:
			binary.LittleEndian.PutUint16(data, uint16(c.probeLen))
			return 2, nil
		case request == uvcGetCur:
			n := copy(data, c.block)
			return n, nil
		case request == uvcSetCur && selector == uvcProbeControl:
			// The camera takes what it was asked for, then overrides the parts
			// it insists on — which is what a real one is entitled to do.
			c.block = append([]byte(nil), data...)
			if c.answerFrame != 0 {
				c.block[3] = byte(c.answerFrame)
			}
			if c.answerInterval != 0 {
				binary.LittleEndian.PutUint32(c.block[4:], c.answerInterval)
			}
			return len(data), nil
		case request == uvcSetCur && selector == uvcCommitControl:
			c.committed = append([]byte(nil), data...)
			return len(data), nil
		}
		return 0, syscall.EINVAL
	}
}

func name(request, selector uint8) string {
	r := map[uint8]string{uvcGetLen: "GET_LEN", uvcGetCur: "GET_CUR", uvcSetCur: "SET_CUR"}[request]
	s := map[uint8]string{uvcProbeControl: "PROBE", uvcCommitControl: "COMMIT"}[selector]
	return r + "/" + s
}

// ⛔ THE SEQUENCE IS THE SPECIFICATION'S, and each step exists for a reason the
// next one depends on: the length is LEARNED rather than guessed from a version
// number, the block is the CAMERA'S so unknown fields survive, and the second
// GET_CUR is the only authority on what will actually arrive.
func TestNegotiateFollowsTheSpecifiedSequence(t *testing.T) {
	_, h := openCamera(t)
	cam := newFakeCamera(t)
	cam.install(t)

	f := UVCFormatDesc{Index: 2, Format: FormatYUY2}
	z := UVCFrameSize{Index: 6, Width: 640, Height: 480, Interval: 400000}
	n, err := h.Negotiate(1, f, z)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	want := []string{"GET_LEN/PROBE", "GET_CUR/PROBE", "SET_CUR/PROBE", "GET_CUR/PROBE", "SET_CUR/COMMIT"}
	if strings.Join(cam.seen, " ") != strings.Join(want, " ") {
		t.Fatalf("the sequence was\n  %v\nwant\n  %v", cam.seen, want)
	}
	if n.Format != 2 || n.Frame != 6 || n.Interval != 400000 {
		t.Fatalf("negotiated %s", n)
	}
	if n.Changed() {
		t.Fatalf("the camera agreed to what it was asked and Changed says otherwise: %s", n)
	}
	// ⛔ THE COMMIT IS THE BLOCK THE CAMERA RETURNED, not the one we composed.
	// Its untouched bytes must still be its own.
	if len(cam.committed) != 34 {
		t.Fatalf("committed %d bytes, want the camera's whole %d-byte block",
			len(cam.committed), 34)
	}
	for i, b := range cam.committed[8:] {
		if b != 0x5A {
			t.Fatalf("byte %d of the committed block is %#02x; the host rewrote a field "+
				"it was not asked about", i+8, b)
		}
	}
}

// ⭐ A CAMERA MAY ANSWER WITH SOMETHING ELSE, and the caller must be told. The
// real device never did, so this is the only place the branch runs.
func TestNegotiateReportsACameraThatChangedTheTerms(t *testing.T) {
	_, h := openCamera(t)
	cam := newFakeCamera(t)
	cam.answerFrame = 1
	cam.answerInterval = 2000000 // 5fps instead of the 25 asked for
	cam.install(t)

	z := UVCFrameSize{Index: 6, Width: 640, Height: 480, Interval: 400000}
	n, err := h.Negotiate(1, UVCFormatDesc{Index: 2}, z)
	if err != nil {
		t.Fatalf("Negotiate: %v", err)
	}
	if !n.Changed() {
		t.Fatalf("the camera answered frame 1 at 5fps and Changed says nothing: %s", n)
	}
	if n.Frame != 1 || n.Interval != 2000000 {
		t.Fatalf("negotiated %s; the ANSWER is the authority, not the request", n)
	}
	if fps := n.FPS(); fps < 4.9 || fps > 5.1 {
		t.Fatalf("FPS = %.3g; the rate must come from what was agreed", fps)
	}
	if !strings.Contains(n.String(), "CHANGED") {
		t.Fatalf("String = %q, which does not warn that the terms moved", n)
	}
}

// Every step of the negotiation can fail, and each failure must say WHICH step:
// "control transfer failed" leaves a reader to guess whether the camera refused
// the format or would not even say how long its block is.
func TestNegotiateNamesTheStepThatFailed(t *testing.T) {
	for _, c := range []struct {
		name string
		fail func(uint8, uint8) error
		says string
	}{
		{"the length", func(r, _ uint8) error {
			if r == uvcGetLen {
				return syscall.EPIPE
			}
			return nil
		}, "probe control's length"},
		{"reading the block", func(r, s uint8) error {
			if r == uvcGetCur {
				return syscall.EPIPE
			}
			return nil
		}, "reading the probe control"},
		{"asking", func(r, s uint8) error {
			if r == uvcSetCur && s == uvcProbeControl {
				return syscall.EPIPE
			}
			return nil
		}, "asking for format"},
		{"committing", func(r, s uint8) error {
			if r == uvcSetCur && s == uvcCommitControl {
				return syscall.EPIPE
			}
			return nil
		}, "committing format"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, h := openCamera(t)
			cam := newFakeCamera(t)
			cam.failAt = c.fail
			cam.install(t)
			_, err := h.Negotiate(1, UVCFormatDesc{Index: 2}, UVCFrameSize{Index: 6, Interval: 400000})
			if !errors.Is(err, syscall.EPIPE) {
				t.Fatalf("Negotiate = %v, want the transfer's own error", err)
			}
			if !strings.Contains(err.Error(), c.says) {
				t.Fatalf("the error says %q, which does not name the step", err)
			}
		})
	}
}

// ⛔ THE READ-BACK IS A SEPARATE STEP AND A SEPARATE FAILURE. A camera that
// took the request and then would not say what it agreed to has told us
// nothing, and committing blindly would stream at a rate nobody knows.
func TestNegotiateRefusesWhenTheCameraWillNotSayWhatItAgreedTo(t *testing.T) {
	_, h := openCamera(t)
	cam := newFakeCamera(t)
	gets := 0
	cam.failAt = func(r, _ uint8) error {
		if r == uvcGetCur {
			gets++
			if gets == 2 {
				return syscall.EIO
			}
		}
		return nil
	}
	cam.install(t)
	_, err := h.Negotiate(1, UVCFormatDesc{Index: 2}, UVCFrameSize{Index: 6, Interval: 400000})
	if !errors.Is(err, syscall.EIO) {
		t.Fatalf("Negotiate = %v", err)
	}
	if !strings.Contains(err.Error(), "agreed to") {
		t.Fatalf("the error says %q, which does not say the read-back failed", err)
	}
}

// A probe block shorter than any UVC version defines, and a camera that answers
// a GET_CUR with less than it promised. Both are refused rather than padded:
// the bytes after a short read are whatever the buffer held.
func TestNegotiateRefusesAnImpossibleProbeBlock(t *testing.T) {
	t.Run("a length no version defines", func(t *testing.T) {
		_, h := openCamera(t)
		cam := newFakeCamera(t)
		cam.probeLen = 12
		cam.install(t)
		_, err := h.Negotiate(1, UVCFormatDesc{Index: 2}, UVCFrameSize{Index: 6, Interval: 400000})
		if !errors.Is(err, ErrBadPayload) {
			t.Fatalf("Negotiate = %v, want ErrBadPayload", err)
		}
		if !strings.Contains(err.Error(), "12 bytes") {
			t.Fatalf("the error says %q, which does not quote what the camera claimed", err)
		}
	})
	t.Run("a short answer", func(t *testing.T) {
		_, h := openCamera(t)
		cam := newFakeCamera(t)
		cam.probeLen = 34
		cam.block = make([]byte, 8) // the camera returns far less than it promised
		cam.install(t)
		_, err := h.Negotiate(1, UVCFormatDesc{Index: 2}, UVCFrameSize{Index: 6, Interval: 400000})
		if !errors.Is(err, ErrShortPayload) {
			t.Fatalf("Negotiate = %v, want ErrShortPayload", err)
		}
	})
}

// Control itself: the request reaches the kernel with the fields the USB
// specification puts in it, and the byte count comes from the ioctl rather than
// from len(data).
func TestControlPassesTheRequestAndReturnsWhatMoved(t *testing.T) {
	_, h := openCamera(t)
	old := ioctlN
	t.Cleanup(func() { ioctlN = old })
	var got usbdevfsCtrlTransfer
	ioctlN = func(_ int, req uintptr, arg unsafe.Pointer) (int, error) {
		if req != usbdevfsControl {
			t.Fatalf("Control used ioctl %#x, want %#x", req, usbdevfsControl)
		}
		got = *(*usbdevfsCtrlTransfer)(arg)
		return 7, nil
	}
	buf := make([]byte, 26)
	n, err := h.Control(ControlIn(), uvcGetCur, uint16(uvcProbeControl)<<8, 1, buf)
	if err != nil {
		t.Fatalf("Control: %v", err)
	}
	// ⛔ WHAT MOVED, NOT WHAT WAS OFFERED. A caller that used len(data) would
	// read nineteen bytes of whatever its buffer held before.
	if n != 7 {
		t.Fatalf("Control returned %d, want the ioctl's 7 rather than len(data)", n)
	}
	if got.RequestType != ControlIn() || got.Request != uvcGetCur {
		t.Fatalf("the request was %#02x/%#02x", got.RequestType, got.Request)
	}
	if got.Value != 0x0100 || got.Index != 1 {
		t.Fatalf("wValue %#04x wIndex %d; the selector is the HIGH byte of wValue and "+
			"wIndex names the interface", got.Value, got.Index)
	}
	if got.Length != 26 || got.Data == 0 {
		t.Fatalf("the transfer offered %d bytes at %#x", got.Length, got.Data)
	}
	if got.Timeout != ControlTimeout {
		t.Fatalf("timeout %d; a control transfer with none can hang the caller forever",
			got.Timeout)
	}

	// A transfer with no buffer names none, rather than pointing at nothing.
	ioctlN = func(_ int, _ uintptr, arg unsafe.Pointer) (int, error) {
		got = *(*usbdevfsCtrlTransfer)(arg)
		return 0, nil
	}
	if _, err := h.Control(ControlOut(), uvcSetCur, 0x0200, 1, nil); err != nil {
		t.Fatalf("Control with no data: %v", err)
	}
	if got.Data != 0 || got.Length != 0 {
		t.Fatalf("an empty transfer carried %d bytes at %#x", got.Length, got.Data)
	}
}

func TestControlReportsTheKernelsRefusal(t *testing.T) {
	_, h := openCamera(t)
	old := ioctlN
	t.Cleanup(func() { ioctlN = old })
	ioctlN = func(int, uintptr, unsafe.Pointer) (int, error) { return 0, syscall.EPIPE }
	if _, err := h.Control(ControlIn(), uvcGetCur, 0x0100, 1, make([]byte, 4)); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("Control = %v, want EPIPE", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := h.Control(ControlIn(), uvcGetCur, 0x0100, 1, nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Control after Close = %v", err)
	}
	if _, err := h.Negotiate(1, UVCFormatDesc{}, UVCFrameSize{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Negotiate after Close = %v", err)
	}
}
