// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"context"
	"fmt"
)

// Cameras returns every camera the camera2 API can see, including external USB
// ones.
//
// ⛔ IT ASKS THE WALL HOST, like [Screens], and for the same reason: listing
// cameras needs NO permission at all. android.permission.CAMERA is required to
// OPEN one, not to be told it is there, so a census costs the user no dialog —
// which is exactly what makes it usable before deciding anything.
//
// An empty list is an answer: it says this phone's HAL offers no external
// camera, which is the thing that decides whether a headset's camera can be
// reached through the platform at all. See [ChooseRoute].
func Cameras(ctx context.Context) ([]Camera, error) {
	return askCensus(ctx, MsgListCameras, MsgCameras, DecodeCameras)
}

// USBDevices returns every device on the phone's USB host port, down to each
// interface's endpoints.
//
// The endpoints are the point: whether a headset's camera can be read through
// Android's Java USB API comes down to whether its UVC streaming endpoints are
// bulk, and nothing but a census says. See [USBDevice.BulkVideo].
//
// It needs no permission either. A USB permission is granted against a device
// to OPEN it; the device list is public.
func USBDevices(ctx context.Context) ([]USBDevice, error) {
	return askCensus(ctx, MsgListUSBDevices, MsgUSBDevices, DecodeUSBDevices)
}

// askCensus dials the wall host, asks one question and drops the connection.
//
// There is no session to keep: a census is a question with an answer rather
// than a stream, and holding a connection open per call would occupy one of the
// host's threads for the life of the process.
func askCensus[T any](ctx context.Context, ask, want uint8, decode func([]byte) ([]T, error)) ([]T, error) {
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

	body, err := s.request(ctx, ask, nil, want)
	if err != nil {
		return nil, err
	}
	return decode(body)
}
