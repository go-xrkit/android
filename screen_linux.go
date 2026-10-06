// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"cmp"
	"context"
	"fmt"
	"syscall"
)

// Screens returns the displays an application may put a [Screen] on: the ones
// the platform will take a Presentation on, minus the built-in panel.
//
// ⛔ IT ASKS THE WALL HOST, NOT THE CAPTURE HOST, and that is the difference
// between needing a MediaProjection and needing nothing. [Displays] goes to the
// capture host because that is where a capture's display list belongs; an
// application that only wants to paint on the glasses would have had to start
// the whole mediaProjection apparatus — a foreground service, a consent dialog,
// a "recording your screen" chip — to find out that a pair of glasses is
// plugged in. This asks the service that already owns Presentations.
//
// The connection is dialled and dropped: there is no session to keep, and a
// display list is a question with an answer rather than a stream.
func Screens(ctx context.Context) ([]Display, error) {
	name := wallSocketName()
	if name == "" {
		return nil, fmt.Errorf("%w: no wall host socket to dial", ErrUnsupported)
	}
	uc, err := dialHost(name)
	if err != nil {
		return nil, fmt.Errorf("%w: dialling the wall host on @%s: %w", ErrUnsupported, name, err)
	}
	s := &session{uc: uc, fc: newFDConn(uc), replies: make(chan reply, repliesDepth), done: make(chan struct{})}
	go s.pump()
	defer s.shutdown(ErrClosed)

	body, err := s.request(ctx, MsgListDisplays, nil, MsgDisplays)
	if err != nil {
		return nil, err
	}
	ds, err := DecodeDisplays(body)
	if err != nil {
		return nil, err
	}
	return presentable(ds), nil
}

// openScreen asks the host for a Presentation on an existing display and maps
// the buffer this process will paint into.
//
// # One connection per screen, for the wall's reasons and one more
//
// It dials the WALL host — the same service and the same socket as
// [Wall.Open]. The two are the same kind of window on the same Android object;
// what differs is which display carries it and which way the pixels go. A
// second service would have been a third copy of the same socket plumbing, and
// the thing that rots.
//
// A connection each, as for a panel, means a screen's whole lifetime is its
// socket's lifetime: closing one cannot disturb another's frames, and a process
// that dies has its Presentations taken down by the kernel closing its sockets
// rather than by bookkeeping that also died.
//
// The one more is the acknowledgements. They flow per screen and pace that
// screen's drawing; routing several screens' acks down one socket would put a
// frame id on the hot path and a bug there stalls a headset.
var openScreen = func(ctx context.Context, d Display, o ScreenOptions) (screenFeed, error) {
	name := wallSocketName()
	if name == "" {
		return screenFeed{}, fmt.Errorf("%w: no wall host socket to dial", ErrUnsupported)
	}
	uc, err := dialHost(name)
	if err != nil {
		return screenFeed{}, fmt.Errorf("%w: dialling the wall host on @%s: %w",
			ErrUnsupported, name, err)
	}
	s := &session{
		uc:      uc,
		fc:      newFDConn(uc),
		replies: make(chan reply, repliesDepth),
		done:    make(chan struct{}),
		// Sized to the largest queue a screen may ask for, so the pump never
		// has to drop an acknowledgement: a dropped one loses its slot for the
		// life of the screen, and with a queue of three that is a third of the
		// frame rate gone with nothing to see.
		acks: make(chan PresentedMessage, MaxQueueDepth),
	}
	go s.pump()

	f, err := s.openScreenOn(ctx, d, o)
	if err != nil {
		s.shutdown(ErrClosed)
		return screenFeed{}, err
	}
	return f, nil
}

// openScreenOn performs the handshake on an already-dialled session: one
// request, a config, then the lent buffer. It is MsgStart's handshake with a
// different request message, and deliberately shares checkConfig with it.
func (s *session) openScreenOn(ctx context.Context, d Display, o ScreenOptions) (screenFeed, error) {
	body, err := s.request(ctx, MsgOpenScreen, EncodeOpenScreen(OpenScreenMessage{
		DisplayID: d.ID,
		Width:     o.Width,
		Height:    o.Height,
		Slots:     o.QueueDepth,
	}), MsgConfig)
	if err != nil {
		return screenFeed{}, err
	}
	cfg, err := DecodeConfig(body)
	if err != nil {
		return screenFeed{}, err
	}
	if err := checkConfig(cfg); err != nil {
		s.stop()
		return screenFeed{}, err
	}
	// The host may not quietly present something other than what was asked
	// for: the application draws against these numbers, and a frame of a
	// different size would be scaled or clipped with no way to notice here.
	if cfg.Width != o.Width || cfg.Height != o.Height {
		s.stop()
		return screenFeed{}, fmt.Errorf("android: asked the host for %dx%d frames and it "+
			"announced %dx%d", o.Width, o.Height, cfg.Width, cfg.Height)
	}

	r, err := s.await(ctx, MsgBuffer)
	if err != nil {
		s.stop()
		return screenFeed{}, err
	}
	slots, slotSize, berr := DecodeBuffer(r.body)
	if berr == nil && (r.fd < 0 || slots != cfg.Slots || slotSize != cfg.SlotSize) {
		berr = fmt.Errorf("android: the host lent %d slots of %d bytes with fd %d, having "+
			"announced %d of %d", slots, slotSize, r.fd, cfg.Slots, cfg.SlotSize)
	}
	if berr != nil {
		if r.fd >= 0 {
			_ = closeFD(r.fd)
		}
		s.stop()
		return screenFeed{}, berr
	}

	buf, err := mapWritable(r.fd, int64(slots)*slotSize)
	if err != nil {
		s.stop()
		return screenFeed{}, err
	}
	return s.screenFeed(cfg, buf), nil
}

// mapWritable maps the frame buffer the host lent, and gives the descriptor
// back.
//
// It is mapped READ-WRITE, which is the opposite of a capture's mapping and the
// whole difference between the two directions: here this process draws the
// pixels and the host reads them. The descriptor is closed once mapped, because
// a mapping outlives the descriptor it came from.
//
// The region on the other side is still the HOST's Android SharedMemory, for
// the reason the capture path documents: an app cannot map a descriptor it
// RECEIVED read-write, so a buffer this process created could not be read by
// the host. That the direction of the pixels reversed does not reverse who may
// create the memory.
func mapWritable(fd int, size int64) ([]byte, error) {
	defer func() { _ = closeFD(fd) }()
	b, err := mmap(fd, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("android: mapping the %d-byte frame buffer the host lent "+
			"for writing: %w", size, err)
	}
	return b, nil
}

// screenFeed packages a dialled session as the transport [Screen] consumes.
func (s *session) screenFeed(cfg ConfigMessage, buf []byte) screenFeed {
	fail := make(chan error, 1)
	go func() {
		<-s.done
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		// cmp.Or rather than an if: a session always ends with a reason, so the
		// fallback is unreachable and a branch there would be a line no test
		// can ever enter. Sending a nil would be worse than any of them — the
		// screen would record "no error" and stop, which reads as healthy.
		fail <- cmp.Or(err, error(ErrClosed))
	}()
	return screenFeed{
		buf:       buf,
		stride:    cfg.Stride,
		slotSize:  cfg.SlotSize,
		slots:     cfg.Slots,
		width:     cfg.Width,
		height:    cfg.Height,
		displayID: cfg.DisplayID,
		present:   func(m PresentMessage) error { return s.send(MsgPresent, EncodePresent(m)) },
		acks:      s.acks,
		fail:      fail,
		close: func() error {
			// MsgBye first so the host takes the Presentation down in its own
			// time, then the socket, which is what releases it if the message
			// never arrived, and only then the mapping: the host must stop
			// reading the buffer before the pages under it are taken away.
			_ = s.send(MsgBye, nil)
			s.shutdown(ErrClosed)
			return munmap(buf)
		},
	}
}
