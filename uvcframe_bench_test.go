// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package android

import (
	"image"
	"testing"
)

// The cost of what a CALLER does between frames, which is the quantity the
// frame-rate deficit was blamed on and which this measures instead of assuming.
//
// ⛔⛔ THE THEORY BEING TESTED IS THAT THE CONSUMER STARVES THE RING. Nothing
// reaps while the caller is away, so any work it does between [UVCStream.Frame]
// calls is time the isochronous endpoint is scheduled with nothing submitted —
// and the packets that arrive then are gone. The ring covers StreamURBs x
// StreamPacketsPerURB microframes, four milliseconds as shipped, while a 640x480
// YUY2 frame takes thirty-two.
//
// That theory already failed one test: raising the ring to sixteen requests
// changed the delivered frames not at all (7968 packets against 7744). If the
// decode is a millisecond or two, it fails the second one too — the ring is
// never starved, and the lost packets are lost somewhere else. Measuring to
// REFUTE one's own explanation is the point.
func BenchmarkDecodeAFrame(b *testing.B) {
	for _, c := range []struct {
		name string
		w, h int
	}{
		{"640x480, what the camera committed to", 640, 480},
		{"1920x1080, what it offers at 5fps", 1920, 1080},
	} {
		b.Run(c.name, func(b *testing.B) {
			a, err := NewFrameAssembler(c.w, c.h)
			if err != nil {
				b.Fatal(err)
			}
			frame := make([]byte, c.w*c.h*BytesPerPixelYUY2)
			for i := range frame {
				frame[i] = byte(i)
			}
			dst := image.NewRGBA(image.Rect(0, 0, c.w, c.h))
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst, err = a.Decode(dst, frame)
				if err != nil {
					b.Fatal(err)
				}
			}
			// ⭐ REPORTED IN MILLISECONDS A FRAME, because that is the unit the
			// question is in: a frame interval at 25 fps is 40 ms and the ring
			// covers 4.
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/frame")
		})
	}
}

// And the cost of ASSEMBLING one, which happens inside the reap loop rather
// than between calls — so it eats the ring directly rather than by starving it.
func BenchmarkAssembleAFrame(b *testing.B) {
	const w, h = 640, 480
	a, err := NewFrameAssembler(w, h)
	if err != nil {
		b.Fatal(err)
	}
	// Payloads the size the camera committed to: 2400 bytes, 12 of them header.
	const payloadBytes = 2400
	const headerLen = 12
	pkt := make([]byte, payloadBytes)
	pkt[0] = headerLen
	whole := w * h * BytesPerPixelYUY2
	perFrame := (whole + payloadBytes - headerLen - 1) / (payloadBytes - headerLen)

	b.SetBytes(int64(whole))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// A whole frame's worth of payloads, then the toggle that ends it.
		for j := 0; j < perFrame; j++ {
			pkt[1] = byte(i % 2) // the frame identifier
			p, err := ParsePayload(pkt)
			if err != nil {
				b.Fatal(err)
			}
			a.Feed(p)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/1e6, "ms/frame")
	b.ReportMetric(float64(perFrame), "packets/frame")
}
