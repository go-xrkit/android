// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import "fmt"

// controlTransfer is the seam the negotiation goes through.
//
// ⛔ IT IS AT THIS LEVEL, NOT AT THE IOCTL. A fake camera installed under the
// ioctl has to reach the caller's buffer through the uintptr in
// usbdevfs_ctrltransfer, and converting that back to a pointer is exactly what
// `go vet` calls a possible misuse: nothing keeps the object alive across the
// round trip, and the suite crashed on it. One step higher, a camera answers on
// a real slice and there is no unsafe in the test at all. Control itself is
// still exercised against the ioctl, where only the REQUEST is read and no
// pointer travels back.
var controlTransfer = (*USBHandle).Control

// Negotiate performs UVC's probe/commit on a streaming interface and returns
// what the camera agreed to.
//
// The sequence is the specification's, and every step of it is a control
// transfer:
//
//  1. GET_LEN on the probe control, so the block's size is LEARNED rather than
//     guessed from a version number. It is 26 bytes in UVC 1.0, 34 in 1.1 and
//     48 in 1.5, and a block of the wrong length is refused outright;
//  2. GET_CUR, to take the camera's own block and leave every field this
//     package does not understand exactly as it was;
//  3. SET_CUR with the format, frame and interval written in;
//  4. GET_CUR again — THE ANSWER. This is where a camera says what it will
//     actually do, which may not be what it was asked;
//  5. SET_CUR on the commit control, which is the same block accepted.
//
// ⛔ THE INTERFACE MUST ALREADY BE CLAIMED. These transfers address the
// streaming interface by number, and a kernel driver still holding it will
// answer them itself or refuse them.
func (h *USBHandle) Negotiate(iface int, f UVCFormatDesc, z UVCFrameSize) (UVCNegotiation, error) {
	wValue := uint16(uvcProbeControl) << 8
	wIndex := uint16(iface)

	// 1. How long is the block?
	var lenBuf [2]byte
	if _, err := controlTransfer(h, ControlIn(), uvcGetLen, wValue, wIndex, lenBuf[:]); err != nil {
		return UVCNegotiation{}, fmt.Errorf("asking the probe control's length: %w", err)
	}
	size := int(lenBuf[0]) | int(lenBuf[1])<<8
	if size < MinProbeBytes {
		return UVCNegotiation{}, fmt.Errorf("%w: the camera says its probe control is %d "+
			"bytes, and no UVC version defines one shorter than %d",
			ErrBadPayload, size, MinProbeBytes)
	}

	// 2. Take the camera's own block.
	block := make(probeBlock, size)
	n, err := controlTransfer(h, ControlIn(), uvcGetCur, wValue, wIndex, block)
	if err != nil {
		return UVCNegotiation{}, fmt.Errorf("reading the probe control: %w", err)
	}
	if n < MinProbeBytes {
		return UVCNegotiation{}, fmt.Errorf("%w: the camera returned %d of %d probe bytes",
			ErrShortPayload, n, size)
	}
	block = block[:n]

	// 3. Ask for what we want.
	block.setFormat(f.Index, z.Index, z.Interval)
	if _, err := controlTransfer(h, ControlOut(), uvcSetCur, wValue, wIndex, block); err != nil {
		return UVCNegotiation{}, fmt.Errorf("asking for format %d frame %d: %w",
			f.Index, z.Index, err)
	}

	// 4. Read what it agreed to, which is the only authority on what follows.
	if _, err := controlTransfer(h, ControlIn(), uvcGetCur, wValue, wIndex, block); err != nil {
		return UVCNegotiation{}, fmt.Errorf("reading back what the camera agreed to: %w", err)
	}
	format, frame, interval, payload := block.format()

	// 5. Commit the block exactly as it came back.
	commit := uint16(uvcCommitControl) << 8
	if _, err := controlTransfer(h, ControlOut(), uvcSetCur, commit, wIndex, block); err != nil {
		return UVCNegotiation{}, fmt.Errorf("committing format %d frame %d: %w",
			format, frame, err)
	}
	return UVCNegotiation{
		Format: format, Frame: frame, Interval: interval,
		MaxPayload: payload, Asked: z,
	}, nil
}
