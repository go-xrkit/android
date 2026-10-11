// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build !linux

package android

// Negotiate performs UVC's probe/commit. Off Android there is no device to
// negotiate with, so it reports [ErrUnsupported].
func (h *USBHandle) Negotiate(int, UVCFormatDesc, UVCFrameSize) (UVCNegotiation, error) {
	return UVCNegotiation{}, ErrUnsupported
}

// Control performs one control transfer. Off Android there is no device.
func (h *USBHandle) Control(uint8, uint8, uint16, uint16, []byte) (int, error) {
	return 0, ErrUnsupported
}
