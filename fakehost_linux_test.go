// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux

package android

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// fake is a host, not a mock: it listens on a real abstract socket, receives a
// real memfd over SCM_RIGHTS and writes real pixels into a real shared mapping.
// The transport under test therefore runs unmodified, which is the only way a
// suite can say anything about it.
type fake struct {
	t    testing.TB
	name string
	ln   *net.UnixListener

	mu     sync.Mutex
	conn   *net.UnixConn
	buf    []byte
	slotSz int64
	slots  int

	// Everything below is written by the TEST goroutine and read by serve, so
	// all of it lives under mu and is reached through set/hooks/state. A hook
	// assigned in the clear is a data race the race detector will find, and
	// finding it in the harness rather than in the package is the harness
	// working as intended.
	displays []Display
	consent  bool
	cfg      ConfigMessage

	// Hooks. A nil hook takes the default behaviour.
	onList    func(*fake)
	onStart   func(*fake, StartMessage)
	onConsent func(*fake, bool)

	gotBuffer chan struct{}
	stopped   chan struct{}
	seq       uint64
}

func newFake(t testing.TB) *fake {
	t.Helper()
	name := fmt.Sprintf("xr-test-%d-%s", os.Getpid(), t.Name())
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fake{
		t: t, name: name, ln: ln,
		displays: []Display{
			{ID: 0, Name: "Built-in Screen", Width: 8, Height: 4, DensityDPI: 420,
				RefreshRate: 60.0004, Flags: FlagSecure},
			{ID: 3, Name: "VITURE Beast", Width: 3840, Height: 1080, DensityDPI: 320,
				RefreshRate: 60, Flags: FlagPresentation},
		},
		consent:   true,
		cfg:       ConfigMessage{Width: 8, Height: 4, Stride: 40, Format: FormatRGBA, Slots: 3, SlotSize: 160},
		gotBuffer: make(chan struct{}, 1),
		stopped:   make(chan struct{}, 8),
	}
	go f.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		f.mu.Lock()
		if f.buf != nil {
			_ = syscall.Munmap(f.buf)
			f.buf = nil
		}
		f.mu.Unlock()
	})
	t.Setenv(EnvSocket, name)
	resetForTest()
	t.Cleanup(resetForTest)
	return f
}

// Every field a test changes after newFake has started serving is written
// through one of these, and read by serve under the same lock. A hook assigned
// in the clear is a data race the detector finds -- and finding it in the
// harness rather than in the package is the harness working as intended.

func (f *fake) setOnList(fn func(*fake)) {
	f.mu.Lock()
	f.onList = fn
	f.mu.Unlock()
}

func (f *fake) setOnStart(fn func(*fake, StartMessage)) {
	f.mu.Lock()
	f.onStart = fn
	f.mu.Unlock()
}

func (f *fake) setOnConsent(fn func(*fake, bool)) {
	f.mu.Lock()
	f.onConsent = fn
	f.mu.Unlock()
}

func (f *fake) setDisplays(ds []Display) {
	f.mu.Lock()
	f.displays = ds
	f.mu.Unlock()
}

func (f *fake) setConsent(v bool) {
	f.mu.Lock()
	f.consent = v
	f.mu.Unlock()
}

func (f *fake) setConfig(c ConfigMessage) {
	f.mu.Lock()
	f.cfg = c
	f.mu.Unlock()
}

// config returns the announced configuration.
func (f *fake) config() ConfigMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fake) serve() {
	c, err := f.ln.AcceptUnix()
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = c
	f.mu.Unlock()

	var pending []byte
	b := make([]byte, 4096)
	oob := make([]byte, syscall.CmsgSpace(4))
	for {
		n, oobn, _, _, err := c.ReadMsgUnix(b, oob)
		if err != nil {
			return
		}
		fd := -1
		if oobn > 0 {
			if scms, err := syscall.ParseSocketControlMessage(oob[:oobn]); err == nil && len(scms) > 0 {
				if fds, err := syscall.ParseUnixRights(&scms[0]); err == nil && len(fds) > 0 {
					fd = fds[0]
				}
			}
		}
		pending = append(pending, b[:n]...)
		for len(pending) >= 5 {
			ln := int(binary.BigEndian.Uint32(pending))
			if len(pending) < 4+ln {
				break
			}
			typ, body := pending[4], append([]byte(nil), pending[5:4+ln]...)
			pending = pending[4+ln:]
			f.handle(typ, body, fd)
			fd = -1
		}
	}
}

func (f *fake) handle(typ uint8, body []byte, fd int) {
	f.mu.Lock()
	onList, onStart, onConsent := f.onList, f.onStart, f.onConsent
	displays := append([]Display(nil), f.displays...)
	consent, cfg := f.consent, f.cfg
	f.mu.Unlock()

	switch typ {
	case MsgListDisplays:
		if onList != nil {
			onList(f)
			return
		}
		f.send(MsgDisplays, EncodeDisplays(displays))
	case MsgConsentRequest:
		prompt := len(body) > 0 && body[0] != 0
		if onConsent != nil {
			onConsent(f, prompt)
			return
		}
		f.send(MsgConsent, EncodeConsent(consent))
	case MsgStart:
		s, err := DecodeStart(body)
		if err != nil {
			f.t.Errorf("decoding MsgStart: %v", err)
			return
		}
		if onStart != nil {
			onStart(f, s)
			return
		}
		f.send(MsgConfig, EncodeConfig(cfg))
		f.lendBuffer(cfg.Slots, cfg.SlotSize)
	case MsgStop, MsgBye:
		select {
		case f.stopped <- struct{}{}:
		default:
		}
	}
}

// lendBuffer creates the shared frame buffer and hands it to the application
// as an ancillary descriptor, which is what the Java host does with an Android
// SharedMemory region. A memfd stands in for that here: the application's side
// of the exchange — receive, map read-only, close — is identical.
func (f *fake) lendBuffer(slots int, slotSize int64) {
	size := int64(slots) * slotSize
	fd, err := unix.MemfdCreate("xr-test", unix.MFD_CLOEXEC)
	if err != nil {
		f.t.Errorf("memfd_create: %v", err)
		return
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Ftruncate(fd, size); err != nil {
		f.t.Errorf("ftruncate: %v", err)
		return
	}
	m, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.t.Errorf("mmap: %v", err)
		return
	}
	f.mu.Lock()
	if f.buf != nil {
		_ = syscall.Munmap(f.buf)
	}
	f.buf, f.slots, f.slotSz, f.seq = m, slots, slotSize, 0
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		_, _, err = c.WriteMsgUnix(FrameMessage(MsgBuffer, EncodeBuffer(slots, slotSize)),
			unix.UnixRights(fd), nil)
		if err != nil {
			f.t.Logf("lending the frame buffer: %v", err)
		}
	}
	select {
	case f.gotBuffer <- struct{}{}:
	default:
	}
}

// sendWithFD sends a message carrying an ancillary descriptor, whatever its
// type. Only MsgBuffer legitimately carries one, so this is how the suite makes
// a host misbehave.
func (f *fake) sendWithFD(typ uint8, body []byte) {
	fd, err := unix.MemfdCreate("xr-stray", unix.MFD_CLOEXEC)
	if err != nil {
		f.t.Errorf("memfd_create: %v", err)
		return
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Ftruncate(fd, 4096); err != nil {
		f.t.Errorf("ftruncate: %v", err)
		return
	}
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		_, _, _ = c.WriteMsgUnix(FrameMessage(typ, body), unix.UnixRights(fd), nil)
	}
}

func (f *fake) send(typ uint8, body []byte) {
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		_, _ = c.Write(FrameMessage(typ, body))
	}
}

// paint writes a distinguishable frame into the next slot and announces it, the
// way the Java host does after its ImageReader hands it an Image.
func (f *fake) paint(fill byte) FrameMsg {
	f.mu.Lock()
	f.seq++
	seq := f.seq
	cfg := f.cfg
	slot := int((seq - 1) % uint64(f.slots))
	off := int64(slot) * f.slotSz
	for i := int64(0); i < int64(cfg.Stride)*int64(cfg.Height); i++ {
		f.buf[off+i] = fill
	}
	f.mu.Unlock()
	m := FrameMsg{Seq: seq, Slot: slot, Width: cfg.Width, Height: cfg.Height,
		Stride: cfg.Stride, AtUnixNano: int64(seq) * 1_000_000}
	f.send(MsgFrame, EncodeFrame(m))
	return m
}

// waitBuffer blocks until the app has handed over its frame buffer.
func (f *fake) waitBuffer() {
	f.t.Helper()
	<-f.gotBuffer
}

// closeConn drops the connection, which is how a host dying reaches the app.
func (f *fake) closeConn() {
	f.mu.Lock()
	c := f.conn
	f.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}
