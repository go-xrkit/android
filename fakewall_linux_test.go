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

// fakeWall stands in for XrWallService: it accepts MANY connections, one per
// owned display, and answers each one's MsgOpenDisplay with a config and a real
// shared buffer over SCM_RIGHTS. It is the same exchange the Java host performs,
// over a real socket and a real memfd rather than a mock.
type fakeWall struct {
	t    testing.TB
	name string
	ln   *net.UnixListener

	mu     sync.Mutex
	panels []*fakePanel
	nextID int
	max    int // the limit the HOST enforces, independently of any Wall
	onOpen func(*fakePanel, OpenDisplayMessage) bool
}

// fakePanel is one connection, and therefore one owned display.
type fakePanel struct {
	w      *fakeWall
	conn   *net.UnixConn
	id     int
	mu     sync.Mutex
	buf    []byte
	slots  int
	slotSz int64
	seq    uint64
	cfg    ConfigMessage
}

func newFakeWall(t testing.TB, max int) *fakeWall {
	t.Helper()
	name := fmt.Sprintf("xr-wall-%d-%s", os.Getpid(), t.Name())
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@" + name, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	w := &fakeWall{t: t, name: name, ln: ln, nextID: 2, max: max}
	go w.accept()
	t.Cleanup(func() {
		_ = ln.Close()
		w.mu.Lock()
		panels := append([]*fakePanel(nil), w.panels...)
		w.mu.Unlock()
		for _, p := range panels {
			p.release()
		}
	})
	t.Setenv(EnvWallSocket, name)
	return w
}

// setOnOpen installs a hook that may answer MsgOpenDisplay itself. Returning
// false takes the default behaviour.
func (w *fakeWall) setOnOpen(fn func(*fakePanel, OpenDisplayMessage) bool) {
	w.mu.Lock()
	w.onOpen = fn
	w.mu.Unlock()
}

// live counts the connections still open, which is how many displays the host
// believes it is serving.
func (w *fakeWall) live() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.panels)
}

func (w *fakeWall) accept() {
	for {
		c, err := w.ln.AcceptUnix()
		if err != nil {
			return
		}
		p := &fakePanel{w: w, conn: c}
		w.mu.Lock()
		w.panels = append(w.panels, p)
		w.mu.Unlock()
		go p.serve()
	}
}

func (w *fakeWall) drop(p *fakePanel) {
	w.mu.Lock()
	for i, q := range w.panels {
		if q == p {
			w.panels = append(w.panels[:i], w.panels[i+1:]...)
			break
		}
	}
	w.mu.Unlock()
	p.release()
}

func (p *fakePanel) release() {
	p.mu.Lock()
	if p.buf != nil {
		_ = syscall.Munmap(p.buf)
		p.buf = nil
	}
	p.mu.Unlock()
	_ = p.conn.Close()
}

func (p *fakePanel) serve() {
	defer p.w.drop(p)
	var pending []byte
	b := make([]byte, 4096)
	for {
		n, err := p.conn.Read(b)
		if err != nil {
			return
		}
		pending = append(pending, b[:n]...)
		for len(pending) >= 5 {
			ln := int(binary.BigEndian.Uint32(pending))
			if len(pending) < 4+ln {
				break
			}
			typ, body := pending[4], append([]byte(nil), pending[5:4+ln]...)
			pending = pending[4+ln:]
			p.handle(typ, body)
		}
	}
}

func (p *fakePanel) handle(typ uint8, body []byte) {
	switch typ {
	case MsgOpenDisplay:
		m, err := DecodeOpenDisplay(body)
		if err != nil {
			p.w.t.Errorf("decoding MsgOpenDisplay: %v", err)
			return
		}
		p.w.mu.Lock()
		hook, max, n := p.w.onOpen, p.w.max, len(p.w.panels)
		p.id = p.w.nextID
		p.w.nextID++
		p.w.mu.Unlock()

		// The host enforces its OWN limit, independently of any Wall. A second
		// process cannot get past it by not using this package.
		if max > 0 && n > max {
			p.send(MsgError, EncodeError(ErrorMessage{
				Code:   codeTooManyDisplays,
				Op:     "createVirtualDisplay",
				Detail: fmt.Sprintf("this host serves at most %d owned displays", max),
			}))
			return
		}
		if hook != nil && hook(p, m) {
			return
		}
		p.openDefault(m)
	case MsgStop, MsgBye:
		// Nothing to unwind in the fake; the connection closing is the release.
	}
}

// openDefault performs the handshake the Java host performs: announce the
// geometry, then lend the frame buffer.
func (p *fakePanel) openDefault(m OpenDisplayMessage) {
	stride := m.Width * 4
	cfg := ConfigMessage{
		Width:     m.Width,
		Height:    m.Height,
		Stride:    stride,
		Format:    FormatRGBA,
		Slots:     m.Slots,
		SlotSize:  int64(stride) * int64(m.Height),
		DisplayID: p.id,
	}
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	p.send(MsgConfig, EncodeConfig(cfg))
	p.lendBuffer(cfg.Slots, cfg.SlotSize)
}

func (p *fakePanel) lendBuffer(slots int, slotSize int64) {
	size := int64(slots) * slotSize
	fd, err := unix.MemfdCreate("xr-wall-test", unix.MFD_CLOEXEC)
	if err != nil {
		p.w.t.Errorf("memfd_create: %v", err)
		return
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Ftruncate(fd, size); err != nil {
		p.w.t.Errorf("ftruncate: %v", err)
		return
	}
	m, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		p.w.t.Errorf("mmap: %v", err)
		return
	}
	p.mu.Lock()
	if p.buf != nil {
		_ = syscall.Munmap(p.buf)
	}
	p.buf, p.slots, p.slotSz, p.seq = m, slots, slotSize, 0
	p.mu.Unlock()
	if _, _, err := p.conn.WriteMsgUnix(FrameMessage(MsgBuffer, EncodeBuffer(slots, slotSize)),
		unix.UnixRights(fd), nil); err != nil {
		p.w.t.Logf("lending the frame buffer: %v", err)
	}
}

func (p *fakePanel) send(typ uint8, body []byte) {
	_, _ = p.conn.Write(FrameMessage(typ, body))
}

// paintSentinel writes the quadrants a real Sentinel Presentation would render,
// so a test can assert on the SAME pixels the live proof asserts on.
func (p *fakePanel) paintSentinel() FrameMsg {
	p.mu.Lock()
	p.seq++
	seq, cfg := p.seq, p.cfg
	slot := int((seq - 1) % uint64(p.slots))
	off := int64(slot) * p.slotSz
	for y := 0; y < cfg.Height; y++ {
		for x := 0; x < cfg.Width; x++ {
			c := SentinelBackground
			switch {
			case x < cfg.Width/2 && y < cfg.Height/2:
				c = SentinelTopLeft
			case x >= cfg.Width/2 && y >= cfg.Height/2:
				c = SentinelBottomRight
			}
			i := off + int64(y)*int64(cfg.Stride) + int64(x)*4
			p.buf[i] = byte(c >> 16)
			p.buf[i+1] = byte(c >> 8)
			p.buf[i+2] = byte(c)
			p.buf[i+3] = 0xFF
		}
	}
	p.mu.Unlock()
	m := FrameMsg{Seq: seq, Slot: slot, Width: cfg.Width, Height: cfg.Height,
		Stride: cfg.Stride, AtUnixNano: int64(seq) * 1_000_000}
	p.send(MsgFrame, EncodeFrame(m))
	return m
}
