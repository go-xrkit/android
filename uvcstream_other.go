// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

import (
	"context"
	"image"
)

// UVCStream is a running camera. Off Android there is none, and every method
// refuses rather than panicking: a compositor links this package on every
// workstation.
type UVCStream struct{ neg UVCNegotiation }

// Stream starts reading frames from a negotiated camera. Off Android there is
// no device, so it reports [ErrUnsupported] — after the portable frame-size
// check, which behaves identically everywhere.
func (h *USBHandle) Stream(neg UVCNegotiation, ep USBEndpoint, w, h2 int) (*UVCStream, error) {
	if _, err := NewFrameAssembler(w, h2); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// Frame reads until one whole frame has been assembled.
func (s *UVCStream) Frame(context.Context) (*image.RGBA, error) { return nil, ErrUnsupported }

// Stats reports what the stream has seen.
func (s *UVCStream) Stats() UVCStreamStats { return UVCStreamStats{} }

// Negotiation is what the camera agreed to for this stream.
func (s *UVCStream) Negotiation() UVCNegotiation { return s.neg }

// Assembler is the frame assembler this stream feeds.
//
// ⛔ nil OFF ANDROID, NOT AN EMPTY ONE. An assembler with no frames in it reads
// as a camera that delivered nothing, which is a different statement from
// "there is no camera on this platform" — and a caller printing a census would
// publish the first while meaning the second.
func (s *UVCStream) Assembler() *FrameAssembler { return nil }

// Close cancels every outstanding request. It is idempotent.
func (s *UVCStream) Close() error { return nil }
