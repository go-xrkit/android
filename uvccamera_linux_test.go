// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// ⚠ uvcDescriptors() RATHER THAN cameraDescriptors(), and the difference is
// the whole open. cameraDescriptors() carries interfaces and an endpoint and NO
// class-specific format descriptors, which is enough for the ioctl tests that
// hand Negotiate a format by hand — and not enough here, where PickFormat has
// to find one. The first run of this file refused with "none of the 0 format(s)
// is one this package can read", which is the fixture speaking and not the code.

// cameraOnTheBus is beastCamera() with the interfaces a census reports, which
// is what awaitCamera looks at: a device is a camera when one of its INTERFACES
// is a UVC streaming one, never because of its device class — this camera's is
// 0xef, "miscellaneous".
func cameraOnTheBus() USBDevice {
	d := beastCamera()
	d.Interfaces = []USBInterface{
		{Number: 0, Class: USBClassVideo, Subclass: USBSubclassVideoControl},
		{Number: 1, Class: USBClassVideo, Subclass: USBSubclassVideoStreaming,
			Endpoints: []USBEndpoint{{Address: 0x81, Attributes: 0x05, MaxPacketSize: 5120}}},
	}
	return d
}

// cameraOnTheBusWithNoStream is the same device with nothing that streams, for
// the refusal that says so.
func cameraOnTheBusWithNoStream() USBDevice {
	d := beastCamera()
	d.Interfaces = []USBInterface{{Number: 0, Class: USBClassVideo, Subclass: USBSubclassVideoControl}}
	return d
}

// agreeableCamera is newFakeCamera with a payload a real camera would answer.
//
// ⛔ THE FILLER IS 0x5A AND dwMaxPayloadTransferSize IS PART OF THE BLOCK, so
// an untouched fake answers a payload of 0x5A5A5A5A — 1 515 870 810 bytes an
// interval — and PickAlternate refuses it, correctly, with "no alternate of
// interface 1 carries 1515870810 bytes an interval". That is what the first run
// of this file reported, and it is the fixture speaking: the negotiation tests
// beside it never read the payload, so nothing had ever needed it to be real.
//
// 2400 is the Beast's own answer, and uvcDescriptors()' later alternate carries
// 5120, so the choice is a real one rather than a coincidence.
func agreeableCamera(t *testing.T) *fakeCamera {
	t.Helper()
	c := newFakeCamera(t)
	binary.LittleEndian.PutUint32(c.block[22:], 2400)
	return c
}

// quietIoctl makes every ioctl succeed, so a test of the OPEN is not also a test
// of the kernel's refusals — those have their own tests.
func quietIoctl(t *testing.T) {
	t.Helper()
	old := ioctl
	t.Cleanup(func() { ioctl = old })
	ioctl = func(int, uintptr, unsafe.Pointer) error { return nil }
}

// noStream replaces the last of the nine steps, so no reaping goroutine starts.
// What Stream itself does has its own fixture; see streamOn.
//
// ⛔ THE STAND-IN CARRIES ITS stop CHANNEL, AND THE FIRST VERSION DID NOT. Close
// closes that channel, and closing a nil one panics — so `&UVCStream{}` crashed
// the cleanup of this very test:
//
//	panic: close of nil channel
//	  (*UVCStream).Close … (*UVCCamera).Close … Cleanup
//
// ⚠ That is the FAKE being unrealistic rather than a defect: a stream only ever
// reaches a caller from h.Stream, which makes its channels before the loop. The
// same zero value is closed safely in uvcstreamother_test.go because Close is a
// stub away from Linux. Guarding the real Close against a state production
// cannot reach would be a speculative branch nothing could cover.
func noStream(t *testing.T) {
	t.Helper()
	old := streamOn
	t.Cleanup(func() { streamOn = old })
	streamOn = func(*USBHandle, UVCNegotiation, USBEndpoint, int, int) (*UVCStream, error) {
		return &UVCStream{stop: make(chan struct{})}, nil
	}
}

// ⭐⭐ THE WHOLE OPEN, IN ORDER, AGAINST A FAKE HOST AND A FAKE CAMERA.
//
// The order is the point of the function: park at the zero-bandwidth alternate,
// negotiate, THEN move away from it — because UVC takes the move away from
// alternate 0 as the signal to start sending. This asserts the negotiation
// really happened and the camera's own answers came back on the Camera.
func TestOpeningTheCameraRunsTheWholeSequence(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setCensus(nil, []USBDevice{cameraOnTheBus()})
	fw.setUSBDescriptors(uvcDescriptors())
	cam := agreeableCamera(t)
	cam.install(t)
	quietIoctl(t)
	noStream(t)

	var said []string
	c, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{},
		func(f string, a ...any) { said = append(said, f) })
	if err != nil {
		t.Fatalf("OpenUVCCamera: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	// The device it opened, and the terms it agreed to.
	if c.Device.Name != cameraOnTheBus().Name {
		t.Errorf("opened %q, want %q", c.Device.Name, cameraOnTheBus().Name)
	}
	if c.Size.Width == 0 || c.Size.Height == 0 {
		t.Errorf("Size = %s, want the frame size it negotiated", c.Size)
	}
	if c.Negotiation.Format == 0 {
		t.Errorf("Negotiation = %s, want the committed block", c.Negotiation)
	}
	if c.Stream() == nil {
		t.Error("Stream is nil on an open camera")
	}

	// ⛔ THE NEGOTIATION IS NOT OPTIONAL, and an open that skipped it would
	// still return a plausible Camera. The fake camera counts what it was
	// asked, so this reads the sequence rather than the result.
	want := "GET_LEN/PROBE GET_CUR/PROBE SET_CUR/PROBE GET_CUR/PROBE SET_CUR/COMMIT"
	if got := strings.Join(cam.seen, " "); got != want {
		t.Errorf("the control sequence was\n  %s\nwant\n  %s", got, want)
	}

	// ⚠ AND THE PROBE'S OWN LINES ARE STILL PRINTED. cmd/xrhead's output is a
	// measurement protocol quoted in the README, so the open logs through the
	// caller's writer rather than swallowing the stages.
	joined := strings.Join(said, "\n")
	for _, line := range []string{"BEFORE", "CAMERA", "FORMAT"} {
		if !strings.Contains(joined, line) {
			t.Errorf("the open did not print a %s line:\n%s", line, joined)
		}
	}
}

// ⛔ A nil logf IS A CALLER THAT DOES NOT WANT THE LINES, not a crash. go-xrkit/desk
// passes nil: a desk's journal has no use for a USB census.
func TestOpeningTheCameraAcceptsNoLogger(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setCensus(nil, []USBDevice{cameraOnTheBus()})
	fw.setUSBDescriptors(uvcDescriptors())
	agreeableCamera(t).install(t)
	quietIoctl(t)
	noStream(t)

	c, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{}, nil)
	if err != nil {
		t.Fatalf("OpenUVCCamera with no logger: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Detached is the replug warning, and an open that took nothing says so.
	if d := c.Detached(); len(d) != 0 {
		t.Errorf("Detached = %v on a fake host that lends rather than claims", d)
	}
}

// ⛔⛔ EVERY STEP'S REFUSAL NAMES ITS STEP. An open is nine calls deep, and
// "could not open the camera" tells a person nothing about which of nine things
// to look at — the measured cost of that was an evening spent on the handshake
// when the answer was a permission.
func TestEveryStepOfTheOpenNamesItself(t *testing.T) {
	for _, c := range []struct {
		name string
		// arrange breaks one step.
		arrange func(t *testing.T, fw *fakeWall, cam *fakeCamera)
		// want is what the error must mention.
		want string
	}{
		{
			name: "nothing on the bus streams video",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				fw.setCensus(nil, []USBDevice{cameraOnTheBusWithNoStream()})
			},
			want: "no UVC camera is attached",
		},
		{
			name: "the descriptor blob is too short to decode",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				fw.setUSBDescriptors([]byte{1, 2, 3})
			},
			// ⚠ "descriptors of", not "interfaces of": the lent descriptor is
			// read as a whole before anything parses it, so a three-byte blob
			// is refused one step earlier than the decode. The first run of
			// this test expected the later step and the error named the
			// earlier one — which is the message doing its job.
			want: "descriptors of",
		},
		{
			name: "the descriptors name no streaming interface",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				// A device descriptor and a control interface, nothing more.
				b := deviceDescriptor()
				b = append(b, 9, descriptorTypeInterface, 0, 0, 1,
					USBClassVideo, USBSubclassVideoControl, 0, 0)
				fw.setUSBDescriptors(b)
			},
			want: "no UVC streaming interface",
		},
		{
			name: "the kernel refuses the claim",
			arrange: func(t *testing.T, _ *fakeWall, _ *fakeCamera) {
				failIoctl(t, usbdevfsClaimInterface)
			},
			want: "claiming interface",
		},
		{
			name: "the kernel refuses the park at alternate 0",
			arrange: func(t *testing.T, _ *fakeWall, _ *fakeCamera) {
				failIoctl(t, usbdevfsSetInterface)
			},
			// ⚠ The park comes BEFORE the move, so the message must be the
			// park's: both go through the same ioctl, and a shared message
			// would leave a reader unable to tell which one failed.
			want: "parking interface",
		},
		{
			// ⛔⛔ THE CASE THE PHONE ACTUALLY PRODUCES. The USB permission is
			// granted per UID, so a newly installed APK has none until the
			// headset is replugged and its own offer accepted — measured on
			// 2026-11-01: `device_permissions` named uid 10350 while the
			// package asking was 10374. This is the refusal a person meets
			// first, and it must say so rather than blame the camera.
			name: "the person has not allowed this application the device",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				fw.setOnOpenUSB(func(p *fakePanel, name string) bool {
					p.send(MsgError, EncodeError(ErrorMessage{
						Code: codeUSBPermissionDenied, Op: "requestPermission",
						Detail: "the user did not allow " + name,
					}))
					return true
				})
			},
			want: "opening /dev/bus/usb",
		},
		{
			// ⛔ A STREAMING INTERFACE WITH NO ALTERNATE 0 IS NOT A CAMERA WITH
			// NO FORMATS. The class descriptors hang off alternate 0; an
			// interface listed only at a later alternate is found by
			// streamingInterface and then has nothing to read, which is its own
			// refusal and not "no format this package can read".
			name: "the streaming interface has no alternate 0",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				b := deviceDescriptor()
				b = append(b, 9, descriptorTypeInterface, 0, 0, 1,
					USBClassVideo, USBSubclassVideoControl, 0, 0)
				b = append(b, 9, descriptorTypeInterface, 1, 6, 1,
					USBClassVideo, USBSubclassVideoStreaming, 0, 0)
				b = append(b, 7, descriptorTypeEndpoint, 0x81, 0x05, 0x00, 0x14, 1)
				fw.setUSBDescriptors(b)
			},
			want: "formats of interface 1",
		},
		{
			// A blob long enough to be read and too malformed to walk: the
			// read and the decode are different steps and each has to name
			// itself.
			name: "the descriptors cannot be walked",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				fw.setUSBDescriptors(append(deviceDescriptor(), 0, descriptorTypeInterface, 0))
			},
			want: "interfaces of",
		},
		{
			// ⚠ THIS IS cameraDescriptors(), the repository's other blob: a
			// streaming interface and an endpoint with NO class-specific
			// format descriptors. It is the fixture that sent the first run of
			// this file off course, and it earns a case of its own.
			name: "the camera lists no format this package can read",
			arrange: func(_ *testing.T, fw *fakeWall, _ *fakeCamera) {
				fw.setUSBDescriptors(cameraDescriptors())
			},
			want: "choosing a format",
		},
		{
			name: "the camera will not say how long its block is",
			arrange: func(_ *testing.T, _ *fakeWall, cam *fakeCamera) {
				cam.failAt = func(request, _ uint8) error {
					if request == uvcGetLen {
						return syscall.EPIPE
					}
					return nil
				}
			},
			want: "negotiating",
		},
		{
			// ⛔ THE PAYLOAD THE FILLER PRODUCES, kept on purpose. An
			// untouched fake answers 0x5A5A5A5A bytes an interval, which no
			// alternate carries — so the refusal a nonsense payload earns is
			// exercised by the very value that made this file fail first.
			name: "no alternate carries the payload the camera asked for",
			arrange: func(_ *testing.T, _ *fakeWall, cam *fakeCamera) {
				binary.LittleEndian.PutUint32(cam.block[22:], 0x5A5A5A5A)
			},
			want: "no alternate of interface",
		},
		{
			// ⛔ THE SECOND SetAltSetting, NOT THE FIRST. Both go through the
			// same ioctl, and the park is step four while the move is step
			// eight: a test that failed the first would never reach this
			// branch, and the two messages would be indistinguishable.
			name: "the kernel refuses the move to the operating alternate",
			arrange: func(t *testing.T, _ *fakeWall, _ *fakeCamera) {
				failNthIoctl(t, usbdevfsSetInterface, 2)
			},
			want: "moving interface",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fw := newFakeWall(t, 0)
			fw.setCensus(nil, []USBDevice{cameraOnTheBus()})
			fw.setUSBDescriptors(uvcDescriptors())
			cam := agreeableCamera(t)
			quietIoctl(t)
			noStream(t)
			c.arrange(t, fw, cam)
			cam.install(t)

			got, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{}, nil)
			if err == nil {
				_ = got.Close()
				t.Fatalf("the open succeeded with %s", c.name)
			}
			if got != nil {
				t.Errorf("a camera came back alongside the refusal: %+v", got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to mention %q", err, c.want)
			}
		})
	}
}

// failIoctl makes one request fail and every other succeed.
func failIoctl(t *testing.T, which uintptr) {
	t.Helper()
	old := ioctl
	t.Cleanup(func() { ioctl = old })
	ioctl = func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req == which {
			return syscall.EBUSY
		}
		return nil
	}
}

// failNthIoctl fails the nth time one request is made, and nothing else.
//
// ⚠ It exists because one ioctl serves two of the nine steps: the park at
// alternate 0 and the move to the operating one. Failing "the set-interface
// ioctl" would only ever reach the first.
func failNthIoctl(t *testing.T, which uintptr, n int) {
	t.Helper()
	old := ioctl
	t.Cleanup(func() { ioctl = old })
	seen := 0
	ioctl = func(_ int, req uintptr, _ unsafe.Pointer) error {
		if req != which {
			return nil
		}
		seen++
		if seen == n {
			return syscall.EBUSY
		}
		return nil
	}
}

// ⚠ THE SEAM'S OWN BODY IS STILL CODE, and replacing it in every other test
// leaves the one line that delegates to h.Stream unexercised — a gap the
// coverage gate finds and a reader would not. Driving it with a negotiation
// that names no format reaches the portable refusal before anything touches a
// descriptor, which is the cheapest honest way through it.
func TestTheStreamSeamDelegatesToTheHandle(t *testing.T) {
	t.Parallel()

	ep := USBEndpoint{Address: 0x81, Attributes: 0x05, MaxPacketSize: 5120, Interval: 1}
	s, err := streamOn(&USBHandle{}, UVCNegotiation{}, ep, 640, 480)
	if !errors.Is(err, ErrInvalidOption) {
		t.Fatalf("streamOn with no format named = %v, want ErrInvalidOption", err)
	}
	if s != nil {
		t.Errorf("a stream came back alongside the refusal: %v", s)
	}
}

// ⛔ Frame IS THE WHOLE POINT OF THE TYPE, and it was the one method nothing
// called: a camera that opens and never hands a frame over is an open nobody
// can use. A cancelled context is the cheapest way to reach it without a live
// stream — the stream answers its context before it answers a camera, which is
// itself a property worth having a caller depend on.
func TestAskingAClosedCameraForAFrameDoesNotBlock(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setCensus(nil, []USBDevice{cameraOnTheBus()})
	fw.setUSBDescriptors(uvcDescriptors())
	agreeableCamera(t).install(t)
	quietIoctl(t)
	noStream(t)

	c, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{}, nil)
	if err != nil {
		t.Fatalf("OpenUVCCamera: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	img, err := c.Frame(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Frame on a cancelled context = %v, want context.Canceled", err)
	}
	if img != nil {
		t.Errorf("a frame came back alongside the refusal: %v", img.Bounds())
	}
}

// ⛔ A STREAM THAT WILL NOT START MUST STILL GIVE THE DEVICE BACK, and that is
// the last and easiest leak to write: an early return past OpenUSB that forgets
// the handle leaves the streaming interface claimed from uvcvideo for the life
// of the process, with no way to ask for it again.
func TestAStreamThatWillNotStartReleasesTheDevice(t *testing.T) {
	fw := newFakeWall(t, 0)
	fw.setCensus(nil, []USBDevice{cameraOnTheBus()})
	fw.setUSBDescriptors(uvcDescriptors())
	agreeableCamera(t).install(t)
	quietIoctl(t)

	old := streamOn
	t.Cleanup(func() { streamOn = old })
	streamOn = func(*USBHandle, UVCNegotiation, USBEndpoint, int, int) (*UVCStream, error) {
		return nil, syscall.ENOMEM
	}

	got, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{}, nil)
	if err == nil {
		_ = got.Close()
		t.Fatal("the open succeeded with a stream that refused to start")
	}
	if got != nil {
		t.Errorf("a camera came back alongside the refusal: %+v", got)
	}
	if !strings.Contains(err.Error(), "starting the stream") {
		t.Errorf("error = %q, want it to name the step that failed", err)
	}

	// ⚠ WHAT THIS TEST DOES NOT ASSERT, AND WHY IT SAYS SO. That the handle
	// went back is not observable from here: the fake host counts panels, and a
	// lent USB descriptor is not one — fw.live() reads 0 before and after. The
	// release is structural instead, through the `fail` helper every branch past
	// OpenUSB returns through, and this case is the one that exercises the last
	// of them. Claiming an assertion that reads a constant would be worse than
	// naming the gap.
}

// ⛔⛔ WAITING IS A CHOICE, AND ZERO IS NOT "WAIT FOR EVER". A desk starting up
// wants the answer now — the glasses are already attached, they are the display
// it is drawing on — while a probe somebody is about to plug a headset into
// wants minutes. The zero value therefore asks once and reports ErrNoCamera,
// and that distinction is what stopped a desk blocking on a cable.
func TestWaitingForACameraIsAChoice(t *testing.T) {
	t.Run("zero asks once and refuses", func(t *testing.T) {
		fw := newFakeWall(t, 0)
		fw.setCensus(nil, nil)

		start := time.Now()
		_, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{}, nil)
		if !errors.Is(err, ErrNoCamera) {
			t.Fatalf("OpenUVCCamera = %v, want ErrNoCamera", err)
		}
		if took := time.Since(start); took > time.Second {
			t.Errorf("a zero wait took %s, which is a wait", took)
		}
		// ⛔ AND THE BARE SENTINEL, with nothing about a budget in it: a caller
		// that did not ask to wait must not be told how long nothing took.
		if strings.Contains(err.Error(), "appeared in") {
			t.Errorf("error = %q, which reports a budget nobody set", err)
		}
	})

	t.Run("a budget says how long it waited", func(t *testing.T) {
		fw := newFakeWall(t, 0)
		fw.setCensus(nil, nil)

		_, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{Wait: 600 * time.Millisecond}, nil)
		if !errors.Is(err, ErrNoCamera) {
			t.Fatalf("OpenUVCCamera = %v, want ErrNoCamera", err)
		}
		if !strings.Contains(err.Error(), "600ms") {
			t.Errorf("error = %q, want it to name the budget it spent", err)
		}
	})

	t.Run("a cancelled wait stops waiting", func(t *testing.T) {
		fw := newFakeWall(t, 0)
		fw.setCensus(nil, nil)

		ctx, cancel := context.WithCancel(screenCtx(t))
		go func() {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}()
		_, err := OpenUVCCamera(ctx, UVCCameraOptions{Wait: time.Minute}, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("OpenUVCCamera = %v, want context.Canceled", err)
		}
	})

	t.Run("a camera that arrives during the wait is taken", func(t *testing.T) {
		fw := newFakeWall(t, 0)
		fw.setCensus(nil, nil)
		fw.setUSBDescriptors(uvcDescriptors())
		agreeableCamera(t).install(t)
		quietIoctl(t)
		noStream(t)

		go func() {
			time.Sleep(100 * time.Millisecond)
			fw.setCensus(nil, []USBDevice{cameraOnTheBus()})
		}()
		c, err := OpenUVCCamera(screenCtx(t), UVCCameraOptions{Wait: 10 * time.Second}, nil)
		if err != nil {
			t.Fatalf("OpenUVCCamera: %v", err)
		}
		_ = c.Close()
	})
}

// ⛔ AND A HOST THAT CANNOT BE ASKED IS NOT AN ABSENT CAMERA. "No camera is
// attached" and "nobody answered" are different states, and collapsing them
// would send somebody looking for a cable when the host had gone.
func TestAHostThatCannotBeAskedIsNotAnAbsentCamera(t *testing.T) {
	// No fake host at all: the socket is not there to connect to.
	t.Setenv(EnvWallSocket, "@this-socket-does-not-exist-"+t.Name())

	_, err := OpenUVCCamera(context.Background(), UVCCameraOptions{}, nil)
	if err == nil {
		t.Fatal("OpenUVCCamera succeeded with no host")
	}
	if errors.Is(err, ErrNoCamera) {
		t.Errorf("error = %q, which blames an absent camera for an absent host", err)
	}
	if !strings.Contains(err.Error(), "listing USB devices") {
		t.Errorf("error = %q, want it to name the step that failed", err)
	}
}
