// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"errors"
	"fmt"
	"image"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// StreamURBs is how many isochronous requests are kept in flight.
//
// ⛔ ONE IS NOT ENOUGH, AND THAT IS NOT A TUNING CHOICE. An isochronous
// endpoint is scheduled every microframe whether anybody is listening: between
// reaping one request and submitting the next, the camera's packets go nowhere.
// With [ISOPacketsPerURB] packets a request, each covers a millisecond, so four
// in flight leave three outstanding while one is being emptied.
const StreamURBs = 4

// A UVCStream is a running camera.
//
// ⛔ IT IS NOT A Stream. The capture and wall paths deliver frames the HOST
// wrote into shared memory and hand them over borrowed; this one is built from
// isochronous packets this process reaped itself, assembled, and decoded. They
// share no machinery and the names are deliberately different.
//
// A UVCStream is NOT safe for concurrent use: the URB ring, the assembler and
// the decode buffer are all single-reader state, and there is no reason for two
// goroutines to read one camera.
type UVCStream struct {
	h    *USBHandle
	ep   USBEndpoint
	asm  *FrameAssembler
	neg  UVCNegotiation
	rgba *image.RGBA

	// ring holds every request this stream owns. A reaped URB is matched to its
	// slot by ADDRESS, compared as a uintptr — never converted back to a
	// pointer, which is what `go vet` calls a misuse and what nothing keeps
	// alive across the round trip.
	ring []*urbSlot

	// pending holds frames that completed and have not been handed out.
	//
	// ⛔⛔ ONE REQUEST CAN COMPLETE TWO FRAMES, AND RETURNING ONLY THE LAST
	// LOSES THE OTHER WHILE STILL COUNTING IT. A request carries
	// ISOPacketsPerURB packets; a frame small enough to fit in fewer than that
	// finishes twice inside one drain, and the first version of this code
	// overwrote `frame` — so Stats said two frames and the caller got one. It
	// does not happen at 640x480, where a frame is some two hundred packets,
	// which is exactly why it had to be found by a test rather than by looking.
	//
	// Frames are whole allocations once complete (see FrameAssembler.complete),
	// so holding them costs nothing but the memory and nothing dangles.
	pending [][]byte

	// late counts frames thrown away because the caller had not come back for
	// the ones before them. It is not guarded: it is written and read on the
	// single reader's goroutine, like pending and the ring.
	late int

	mu      sync.Mutex
	closed  bool
	packets int
	bytes   int
	empty   int
}

// urbSlot is one request and the memory it names.
type urbSlot struct {
	// raw is the URB and its packet descriptors in ONE allocation, because the
	// kernel reads them as one struct with a flexible array at the end.
	raw []byte
	// buf is where the packets land.
	buf []byte
	// addr is raw's address, kept so a reaped URB can be recognised without
	// turning the kernel's answer back into a pointer.
	addr uintptr
	// out says whether this slot is currently submitted.
	out bool
}

// Stream starts reading frames from a camera that has already been negotiated.
//
// ⛔ THE ORDER MATTERS AND NOTHING ENFORCES IT BUT THIS SENTENCE: the interface
// must be claimed, its alternate selected and the format committed before this
// is called. A stream started before the commit reaps packets of nothing, which
// looks exactly like a camera with the lens covered.
func (h *USBHandle) Stream(neg UVCNegotiation, ep USBEndpoint, w, h2 int) (*UVCStream, error) {
	asm, err := NewFrameAssembler(w, h2)
	if err != nil {
		return nil, err
	}
	packet := ep.ISOPacketSize()
	if packet <= 0 {
		return nil, fmt.Errorf("%w: endpoint %#02x carries %d bytes an interval",
			ErrInvalidOption, ep.Address, packet)
	}
	s := &UVCStream{h: h, ep: ep, asm: asm, neg: neg}
	for i := 0; i < StreamURBs; i++ {
		slot := newURBSlot(ep, packet)
		s.ring = append(s.ring, slot)
		// ⛔ PINNED BEFORE IT IS SUBMITTED, because after the submit the kernel
		// is already entitled to write into it. See USBHandle.pinned.
		h.pin(slot)
		if err := s.submit(slot); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	return s, nil
}

// newURBSlot allocates one request.
func newURBSlot(ep USBEndpoint, packet int) *urbSlot {
	n := ISOPacketsPerURB
	s := &urbSlot{
		buf: make([]byte, packet*n),
		raw: make([]byte, urbSize+isoPacketSize*n),
	}
	s.addr = uintptr(unsafe.Pointer(&s.raw[0]))
	u := (*usbdevfsURB)(unsafe.Pointer(&s.raw[0]))
	u.Type = uint8(urbTypeFor(ep.Type()))
	u.Endpoint = uint8(ep.Address)
	u.Buffer = uintptr(unsafe.Pointer(&s.buf[0]))
	u.BufferLength = int32(len(s.buf))
	u.NumberOfPackets = int32(n)
	for i := 0; i < n; i++ {
		p := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&s.raw[urbSize+isoPacketSize*i]))
		p.Length = uint32(packet)
	}
	return s
}

// The three operations a stream performs on the kernel, as seams.
//
// ⛔⛔ THEY ARE AT THIS LEVEL, NOT AT THE IOCTL, AND FOR THE SAME REASON AS
// [controlTransfer]. A fake camera installed under the ioctl is handed
// unsafe.Pointer(&raw[0]) and nothing else: to put packets where the stream will
// read them it has to take the Buffer field — a uintptr — and turn it back into
// a pointer, which is the unsafe misuse `go vet` refuses and which already
// crashed this suite once. One step higher a fake is handed the *urbSlot, whose
// buf is an ordinary Go slice, and writes into it with no unsafe at all.
//
// The ioctl numbers themselves are still asserted against usbdevice_fs.h in
// usbfs_test.go, and [USBHandle.SubmitISO] still exercises the real path.
var (
	submitURB = func(fd int, slot *urbSlot) error {
		return ioctl(fd, usbdevfsSubmitURB, unsafe.Pointer(&slot.raw[0]))
	}
	reapURB = func(fd int) (uintptr, error) {
		var p uintptr
		err := ioctl(fd, usbdevfsReapURBNDelay, unsafe.Pointer(&p))
		return p, err
	}
	discardURB = func(fd int, slot *urbSlot) error {
		return ioctl(fd, usbdevfsDiscardURB, unsafe.Pointer(&slot.raw[0]))
	}
)

// submit queues one slot. The caller holds no lock; the ring is single-reader.
func (s *UVCStream) submit(slot *urbSlot) error {
	if err := submitURB(s.h.fd, slot); err != nil {
		return fmt.Errorf("android: submitting an isochronous request to %#02x of %s: %w",
			s.ep.Address, s.h.name, err)
	}
	slot.out = true
	runtime.KeepAlive(slot.buf)
	runtime.KeepAlive(slot.raw)
	return nil
}

// Frame reads until one whole frame has been assembled, and returns it decoded.
//
// The image is REUSED between calls, so a steady stream allocates nothing after
// the first frame. A caller that keeps one copies it.
//
// It reports the context's error if the wait is abandoned, and [ErrClosed]
// after [UVCStream.Close].
func (s *UVCStream) Frame(ctx context.Context) (*image.RGBA, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}

		// A frame a previous request left over is handed out before the kernel
		// is asked for more, or it would be held while fresher ones arrive.
		if len(s.pending) > 0 {
			frame := s.pending[0]
			s.pending = s.pending[1:]
			// decodeInto rather than Decode: a queued frame is exactly a
			// frame's worth by construction, and the length check on the
			// exported entry is dead here — see FrameAssembler.decodeInto.
			s.rgba = s.asm.decodeInto(s.rgba, frame)
			return s.rgba, nil
		}

		slot, err := s.reap()
		if err != nil {
			return nil, err
		}
		if slot == nil {
			// Nothing has completed. ⚠ THE SLEEP IS THE SCHEDULE, not a
			// guess: at ISOPacketsPerURB packets a request each request covers
			// a millisecond, so there is nothing to collect sooner and a busy
			// loop would burn a core to learn that.
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Millisecond):
			}
			continue
		}

		s.drain(slot)
		if err := s.submit(slot); err != nil {
			return nil, err
		}
	}
}

// reap collects one completed request, or nil when none has.
func (s *UVCStream) reap() (*urbSlot, error) {
	p, err := reapURB(s.h.fd)
	if err != nil {
		if errors.Is(err, syscall.EAGAIN) {
			return nil, nil // nothing finished yet
		}
		return nil, fmt.Errorf("android: reaping a request from %s: %w", s.h.name, err)
	}
	// ⛔ MATCHED BY ADDRESS, COMPARED AS A NUMBER. The kernel hands back the
	// pointer it was given; converting it back to a *usbdevfsURB would be the
	// unsafe misuse `go vet` refuses, and the slot already knows its own
	// memory.
	for _, slot := range s.ring {
		if slot.addr == p {
			slot.out = false
			return slot, nil
		}
	}
	return nil, fmt.Errorf("android: %s completed a request at %#x that this stream does "+
		"not own", s.h.name, p)
}

// drain reads one completed request's packets into the assembler, queueing
// every frame that completed.
func (s *UVCStream) drain(slot *urbSlot) {
	off := 0
	packet := s.ep.ISOPacketSize()
	for i := 0; i < ISOPacketsPerURB; i++ {
		d := (*usbdevfsISOPacketDesc)(unsafe.Pointer(&slot.raw[urbSize+isoPacketSize*i]))
		n := int(d.ActualLength)
		pkt := slot.buf[off : off+min(n, packet)]
		off += packet

		s.mu.Lock()
		s.packets++
		s.bytes += n
		if n == 0 {
			// ⚠ AN EMPTY PACKET IS NORMAL AND IS COUNTED. An isochronous
			// endpoint is scheduled whether the camera has anything to send, so
			// most packets are empty between frames; a stream reporting only
			// bytes would look stalled when it is merely idle.
			s.empty++
		}
		s.mu.Unlock()
		if n == 0 {
			continue
		}

		pl, err := ParsePayload(pkt)
		if err != nil {
			// A packet this process cannot read is not a frame boundary and
			// not pixels. Counting it as bad would blame the camera for our
			// parse; it is simply skipped, and the frame it belonged to will
			// come out short and be dropped.
			continue
		}
		if f, ok := s.asm.Feed(pl); ok {
			s.pending = append(s.pending, f)
		}
	}
	// ⛔ THE QUEUE HAS A CEILING AND IT DROPS THE OLDEST. A caller slower than
	// the camera would otherwise grow this without bound, and the consumer here
	// is head tracking: the FRESHEST frame is the one worth having, and a stale
	// one delivered late is worse than none. Dropped frames are counted, so a
	// caller that is too slow can SEE that it is.
	for len(s.pending) > StreamURBs {
		s.pending = s.pending[1:]
		s.late++
	}
}

// Stats reports what the stream has seen.
func (s *UVCStream) Stats() UVCStreamStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, d, b := s.asm.Stats()
	return UVCStreamStats{
		Packets: s.packets, Empty: s.empty, Bytes: s.bytes,
		Frames: f, Dropped: d, Bad: b, Late: s.late,
	}
}

// Negotiation is what the camera agreed to for this stream.
func (s *UVCStream) Negotiation() UVCNegotiation { return s.neg }

// Close cancels every outstanding request. It is idempotent.
//
// ⛔ IT DISCARDS WHAT IS STILL IN FLIGHT. A request left outstanding has the
// kernel writing into this process's memory on the camera's schedule, and the
// Go heap is entitled to move on without it.
func (s *UVCStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	for _, slot := range s.ring {
		if slot.out {
			_ = discardURB(s.h.fd, slot)
			slot.out = false
		}
	}
	return nil
}
