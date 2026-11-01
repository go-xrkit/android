// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"context"
	"errors"
	"fmt"
	"image"
	"time"
)

// UVCCamera is a headset's UVC camera, open and streaming.
//
// ⭐⭐ IT EXISTS SO THE SEQUENCE HAS ONE HOME. Opening one of these is nine
// steps in a fixed order, and three of them are only correct in that order —
// park at the zero-bandwidth alternate, negotiate, THEN move away from it,
// because UVC takes the move away from alternate 0 as the signal to start
// sending. Fifteen seconds of empty packets taught that, and it was written
// once, inside cmd/xrhead, where nothing else could reach it.
//
// ⚠ go-xrkit/desk needs exactly this to follow a head on Android — the glasses
// publish no orientation, so the camera is the only road — and a second copy of
// a nine-step order is how one of the copies ends up wrong.
//
// ⛔ OPENING IT TAKES THE INTERFACE FROM uvcvideo AND DOES NOT GIVE IT BACK.
// Close says so through [Camera.Detached], and the remedy is a replug. That is
// a property of the phone, not of this code: the reconnect ioctl does not
// restore the binding here.
type UVCCamera struct {
	h *USBHandle
	s *UVCStream

	// Device is the camera this opened.
	Device USBDevice
	// Format and Size are what the camera agreed to.
	Format UVCFormatDesc
	Size   UVCFrameSize
	// Negotiation is the committed probe/commit result, payload included.
	Negotiation UVCNegotiation
}

// ErrNoCamera says no UVC camera was attached. It is not a failure to open one:
// nothing was there to open.
var ErrNoCamera = errors.New("android: no UVC camera is attached")

// OpenUVCCamera finds the headset's camera and starts its stream.
//
// logf, when not nil, is given the same stage lines cmd/xrhead printed when it
// owned this sequence — they are a measurement protocol, quoted in the
// repository's README, and collapsing them into one error would lose which
// stage a refusal came from.
func OpenUVCCamera(ctx context.Context, o UVCCameraOptions, logf func(string, ...any)) (*UVCCamera, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	dev, err := awaitCamera(ctx, o.Wait, logf)
	if err != nil {
		return nil, err
	}
	logf("CAMERA %s %04x:%04x %q", dev.Name, dev.VendorID, dev.ProductID, dev.Product)

	h, err := OpenUSB(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("android: opening %s: %w", dev.Name, err)
	}
	// ⛔ EVERY FAILURE PAST THIS POINT MUST GIVE THE HANDLE BACK. An early
	// return that leaked it would leave the interface claimed from uvcvideo for
	// the life of the process, with no way to ask for it again.
	fail := func(err error) (*UVCCamera, error) {
		_ = h.Close()
		return nil, err
	}

	desc, err := h.Descriptors()
	if err != nil {
		return fail(fmt.Errorf("android: descriptors of %s: %w", dev.Name, err))
	}
	ifaces, err := desc.Interfaces()
	if err != nil {
		return fail(fmt.Errorf("android: interfaces of %s: %w", dev.Name, err))
	}
	iface, ok := streamingInterface(ifaces)
	if !ok {
		return fail(fmt.Errorf("android: no UVC streaming interface on %s", dev.Name))
	}
	if err := h.ClaimInterface(iface); err != nil {
		return fail(fmt.Errorf("android: claiming interface %d of %s: %w", iface, dev.Name, err))
	}
	// ⛔ ZERO BANDWIDTH WHILE NEGOTIATING, and the operating alternate only
	// after the commit: UVC takes the move away from alternate 0 as the signal
	// to start sending, so arriving there first means it never happens.
	if err := h.SetAltSetting(iface, ZeroBandwidthAlternate); err != nil {
		return fail(fmt.Errorf("android: parking interface %d at alternate 0: %w", iface, err))
	}

	formats, err := desc.UVCFormats(iface)
	if err != nil {
		return fail(fmt.Errorf("android: formats of interface %d: %w", iface, err))
	}
	want, size, err := PickFormat(formats)
	if err != nil {
		return fail(fmt.Errorf("android: choosing a format on %s: %w", dev.Name, err))
	}
	neg, err := h.Negotiate(iface, want, size)
	if err != nil {
		return fail(fmt.Errorf("android: negotiating %s %s: %w", want.Format, size, err))
	}
	logf("FORMAT %s %s, payload %d", want.Format, size, neg.MaxPayload)

	alt, ep, ok := PickAlternate(ifaces, iface, neg.MaxPayload)
	if !ok {
		return fail(fmt.Errorf("android: no alternate of interface %d carries %d bytes an interval",
			iface, neg.MaxPayload))
	}
	if err := h.SetAltSetting(alt.Number, alt.Alternate); err != nil {
		return fail(fmt.Errorf("android: moving interface %d to alternate %d: %w",
			alt.Number, alt.Alternate, err))
	}

	s, err := h.Stream(neg, ep, size.Width, size.Height)
	if err != nil {
		return fail(fmt.Errorf("android: starting the stream on %s: %w", dev.Name, err))
	}
	return &UVCCamera{
		h: h, s: s, Device: dev,
		Format: want, Size: size, Negotiation: neg,
	}, nil
}

// Frame is the next whole frame, decoded.
func (c *UVCCamera) Frame(ctx context.Context) (*image.RGBA, error) { return c.s.Frame(ctx) }

// Stream is the stream underneath, for a caller that wants its statistics.
func (c *UVCCamera) Stream() *UVCStream { return c.s }

// Detached names the interfaces taken from their kernel driver and not given
// back, which on this phone means the camera has no driver until the headset is
// replugged. Empty when nothing was taken.
func (c *UVCCamera) Detached() []int { return c.h.Detached() }

// Close stops the stream and releases the device.
//
// ⚠ THE STREAM FIRST. Closing the handle under a running stream leaves its
// reaping goroutine submitting requests against a descriptor that is gone.
func (c *UVCCamera) Close() error {
	serr := c.s.Close()
	herr := c.h.Close()
	return errors.Join(serr, herr)
}

// awaitCamera waits for a UVC device to appear, taking a census first.
//
// ⛔ THE CENSUS COMES FIRST AND NEEDS NO PERMISSION. Listing devices is not
// opening one, and reporting what was attached BEFORE the wait is what turns
// "no camera appeared" into "here is what was there instead".
func awaitCamera(ctx context.Context, budget time.Duration,
	logf func(string, ...any)) (USBDevice, error) {

	deadline := time.Now().Add(budget)
	first := true
	for {
		devs, err := USBDevices(ctx)
		if err != nil {
			return USBDevice{}, fmt.Errorf("android: listing USB devices: %w", err)
		}
		if first {
			logf("BEFORE %d USB device(s) attached:", len(devs))
			for _, d := range devs {
				logf("  %s %04x:%04x %q", d.Name, d.VendorID, d.ProductID, d.Product)
			}
			first = false
		}
		for _, d := range devs {
			if len(d.VideoStreaming()) > 0 {
				return d, nil
			}
		}
		if !time.Now().Before(deadline) {
			if budget <= 0 {
				return USBDevice{}, ErrNoCamera
			}
			return USBDevice{}, fmt.Errorf("%w: none appeared in %s", ErrNoCamera, budget)
		}
		select {
		case <-ctx.Done():
			return USBDevice{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
