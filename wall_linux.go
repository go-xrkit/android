// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"fmt"
)

// WallAvailable reports whether this process is running under a host that
// serves owned displays. It dials nothing.
func WallAvailable() bool { return wallSocketName() != "" }

// wallSocketName is the abstract socket the wall host listens on: whatever the
// host named, or the name derived from this process's package.
func wallSocketName() string {
	env, ok := lookupEnv(EnvWallSocket)
	home, _ := lookupEnv("HOME")
	return DeriveWallSocket(env, ok, home)
}

// openOwnedDisplay asks the wall host for one display and returns its feed.
//
// # One connection per display, deliberately
//
// The capture path memoises a single process-wide session, because there is one
// screen and one projection. A wall is the opposite: several displays live at
// once, each producing frames independently, and each needs its own shared
// buffer and its own MsgFrame stream. Multiplexing them down one socket would
// mean adding a feed id to every frame message and routing it — new state on
// the hot path, and a bug there mixes two panels' pixels.
//
// A connection each costs one socket per panel, which for the six-to-eight a
// ribbon uses is nothing, and buys three things: [Stream] is reused UNCHANGED
// including its tested frame plumbing, a display's whole lifetime is exactly a
// socket's lifetime so closing one cannot disturb another, and a feed that
// fails is torn down by the kernel rather than by bookkeeping.
func openOwnedDisplay(ctx context.Context, spec DisplaySpec) (feed, error) {
	name := wallSocketName()
	if name == "" {
		return feed{}, fmt.Errorf("%w: no wall host socket to dial", ErrUnsupported)
	}
	uc, err := dialHost(name)
	if err != nil {
		return feed{}, fmt.Errorf("%w: dialling the wall host on @%s: %w", ErrUnsupported, name, err)
	}
	s := &session{uc: uc, fc: newFDConn(uc), replies: make(chan reply, 1), done: make(chan struct{})}
	go s.pump()

	st, id, err := s.openDisplay(ctx, spec)
	if err != nil {
		s.shutdown(ErrClosed)
		return feed{}, err
	}
	// Closing the display must close its SOCKET, not merely stop its stream.
	// Stream.Close sends MsgStop and unmaps, which is right for the capture
	// path where the session is process-wide and outlives any one stream. Here
	// the session IS the display: leaving the socket open would leave the host
	// holding a VirtualDisplay for a panel the caller has released, and the
	// limit that keeps this feature from rebooting the phone counts those.
	return feed{stream: st, id: id, close: func() error {
		err := st.Close()
		s.shutdown(ErrClosed)
		return err
	}}, nil
}

// openDisplay performs the two-message handshake on an already-dialled wall
// session. It is the MsgStart handshake with a different request message, and
// deliberately shares checkConfig and the buffer handover with it.
func (s *session) openDisplay(ctx context.Context, spec DisplaySpec) (*Stream, int, error) {
	body, err := s.request(ctx, MsgOpenDisplay, EncodeOpenDisplay(OpenDisplayMessage{
		Width:          spec.Width,
		Height:         spec.Height,
		DensityDPI:     spec.DensityDPI,
		Slots:          spec.QueueDepth,
		ContentKind:    spec.Content.kind(),
		ContentPayload: spec.Content.payload(),
	}), MsgConfig)
	if err != nil {
		return nil, 0, err
	}
	cfg, err := DecodeConfig(body)
	if err != nil {
		return nil, 0, err
	}
	if err := checkConfig(cfg); err != nil {
		s.stop()
		return nil, 0, err
	}

	r, err := s.await(ctx, MsgBuffer)
	if err != nil {
		s.stop()
		return nil, 0, err
	}
	slots, slotSize, berr := DecodeBuffer(r.body)
	if berr == nil && (r.fd < 0 || slots != cfg.Slots || slotSize != cfg.SlotSize) {
		berr = fmt.Errorf("android: the wall host lent %d slots of %d bytes with fd %d, "+
			"having announced %d of %d", slots, slotSize, r.fd, cfg.Slots, cfg.SlotSize)
	}
	if berr != nil {
		if r.fd >= 0 {
			_ = closeFD(r.fd)
		}
		s.stop()
		return nil, 0, berr
	}

	st := &Stream{s: s, opts: Options{
		Width:      cfg.Width,
		Height:     cfg.Height,
		FPS:        DefaultFPS,
		QueueDepth: cfg.Slots,
	}, cfg: cfg, wake: make(chan struct{}, 1)}
	if err := st.mapBuffer(r.fd); err != nil {
		s.stop()
		return nil, 0, err
	}
	s.mu.Lock()
	s.stream = st
	s.mu.Unlock()
	return st, cfg.DisplayID, nil
}
