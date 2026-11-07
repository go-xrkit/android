// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"strings"
)

// PageSize is the alignment a native library gets, in bytes.
//
// ⛔ 16 KiB, NOT 4. build-tools 35.0.0's zipalign says of its own older flag:
// "-p: 4kb page-align uncompressed .so files" and "-P <pagesize_kb>: Align
// uncompressed .so files to the specified page size. Valid values are 4, 16 and
// 64" — and developer.android.com/tools/zipalign says "It is recommended to use
// -P 16 instead, as -p is deprecated" (read 2026-11-07). A device with 16 KiB
// pages cannot map a library aligned to 4.
const PageSize = 16 * 1024

// wordAlign is what every other stored entry gets: resources.arsc is read by
// mmap from API 30, and zipalign's positional <align> argument is 4 for exactly
// this reason.
const wordAlign = 4

// alignExtraID is the extra-field id zipalign uses for its padding, chosen from
// the "unofficial" range. Writing the same one means `zipalign -c` and this
// package agree about which bytes are padding.
const alignExtraID = 0xd935

// Add is a file spliced into the APK aapt2 produced.
type Add struct {
	Name  string // the path inside the APK, e.g. "lib/arm64-v8a/libxrapp.so"
	Data  []byte
	Store bool // true: stored uncompressed, and therefore aligned
}

// Repack copies every entry of src into dst, splices in adds, and aligns every
// stored entry — a .so to PageSize, anything else to wordAlign.
//
// ⛔⛔ THE ALIGNMENT IS WHY THIS IS NOT `zip` FOLLOWED BY `zipalign`. zipalign
// aligns entries that are STORED; `zip` deflates by default; so the payload was
// compressed, zipalign had nothing to align, and reported "OK - compressed",
// which reads like a pass. Storing the library is the point and the alignment
// follows from it: see Verify, which refuses a compressed .so rather than
// reporting OK about it.
//
// Every entry is written with CreateRaw and no data descriptor, so a local
// header is exactly 30 + len(name) + len(extra) bytes and the next offset is
// arithmetic rather than a guess. Writing through CreateHeader would have
// archive/zip append a nine-byte extended-timestamp extra field of its own
// (writer.go, "Use \"extended timestamp\" format since this is what Info-ZIP
// uses"), which would silently move every data offset this code had just
// computed.
func Repack(src, dst string, adds []Add) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return fmt.Errorf("reading %s, which aapt2 should have written: %w", src, err)
	}
	defer func() { _ = r.Close() }()

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := newAligningWriter(f)
	taken := make(map[string]bool, len(r.File)+len(adds))
	for _, e := range r.File {
		taken[e.Name] = true
		if err := w.copyRaw(e); err != nil {
			return fmt.Errorf("copying %s: %w", e.Name, err)
		}
	}
	for _, a := range adds {
		// ⛔ A SECOND COPY OF A NAME IS NOT AN OVERWRITE. A zip may hold two
		// entries with the same name and readers disagree about which wins, so
		// Android's would not necessarily be the one written last.
		if taken[a.Name] {
			return fmt.Errorf("%s is already in %s: aapt2 packaged it, so splicing a second "+
				"copy would leave two entries of that name and no rule about which the "+
				"platform reads", a.Name, path.Base(src))
		}
		taken[a.Name] = true
		if err := w.add(a); err != nil {
			return fmt.Errorf("adding %s: %w", a.Name, err)
		}
	}
	if err := w.close(); err != nil {
		return err
	}
	return f.Close()
}

// aligningWriter tracks the archive offset itself, because archive/zip does not
// expose it and the buffer it writes through makes an outer counter lag.
type aligningWriter struct {
	z   *zip.Writer
	off int64
}

func newAligningWriter(w io.Writer) *aligningWriter {
	return &aligningWriter{z: zip.NewWriter(w)}
}

func (a *aligningWriter) close() error { return a.z.Close() }

// copyRaw copies one entry without recompressing it.
func (a *aligningWriter) copyRaw(e *zip.File) error {
	src, err := e.OpenRaw()
	if err != nil {
		return err
	}
	h := e.FileHeader // a copy: the reader's header must not be mutated
	return a.write(&h, src, int64(e.CompressedSize64))
}

// add splices in one new file, compressing it here unless it is to be stored.
func (a *aligningWriter) add(f Add) error {
	h := &zip.FileHeader{
		Name: f.Name,
		// ⚠ A FIXED 1980-01-01, not time.Now(): two builds of the same sources
		// should differ in no byte, and Modified is left zero so that
		// archive/zip writes these DOS fields and appends no extra field.
		ModifiedDate: 0x0021, // year 1980, month 1, day 1
		ModifiedTime: 0,
	}
	h.CRC32 = crc32.ChecksumIEEE(f.Data)
	h.UncompressedSize64 = uint64(len(f.Data))
	data := f.Data
	if f.Store {
		h.Method = zip.Store
	} else {
		h.Method = zip.Deflate
		var buf bytes.Buffer
		zw, err := flate.NewWriter(&buf, flate.BestCompression)
		if err != nil {
			return err
		}
		if _, err := zw.Write(f.Data); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		data = buf.Bytes()
	}
	h.CompressedSize64 = uint64(len(data))
	return a.write(h, bytes.NewReader(data), int64(len(data)))
}

// write emits one entry, padding its local header so the DATA lands on the
// alignment its name and method ask for.
func (a *aligningWriter) write(h *zip.FileHeader, data io.Reader, n int64) error {
	// ⛔ NO DATA DESCRIPTOR. CreateRaw writes the real sizes into the local
	// header only when the flag is clear, and a descriptor would add sixteen
	// bytes after the data that this arithmetic does not know about. The sizes
	// are known here in every case: a copied entry carries them in the central
	// directory, and a new one was just measured.
	h.Flags &^= 0x8
	// Modified is zeroed so archive/zip does not append its own extra field,
	// which would move the data offset computed just below. The DOS fields a
	// copied entry carries are preserved, so the timestamp survives.
	h.Modified = zeroTime
	h.Extra = stripAlignExtra(h.Extra)

	if h.Method == zip.Store {
		want := alignmentFor(h.Name)
		head := a.off + int64(fileHeaderLen+len(h.Name)+len(h.Extra))
		if pad := padding(head, want); pad > 0 {
			h.Extra = append(h.Extra, alignExtra(pad)...)
		}
	}
	hdr := int64(fileHeaderLen + len(h.Name) + len(h.Extra))
	w, err := a.z.CreateRaw(h)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, data); err != nil {
		return err
	}
	a.off += hdr + n
	return nil
}

// fileHeaderLen is the fixed part of a local file header: signature, versions,
// flags, method, time, date, crc, both sizes, and the two length fields
// (APPNOTE.TXT 4.3.7).
const fileHeaderLen = 30

// alignmentFor is the alignment a stored entry's data must start on.
func alignmentFor(name string) int {
	if strings.HasSuffix(name, ".so") {
		return PageSize
	}
	return wordAlign
}

// padding is how many bytes to insert before data at offset head so that it
// lands on a multiple of want.
//
// ⚠ An extra field cannot be shorter than its own four-byte header, so a
// shortfall of one to three bytes is paid for with a whole further alignment —
// which is what zipalign does too.
func padding(head int64, want int) int {
	if want <= 1 {
		return 0
	}
	pad := int((int64(want) - head%int64(want)) % int64(want))
	for pad != 0 && pad < 4 {
		pad += want
	}
	return pad
}

// alignExtra builds a padding extra field of exactly n bytes.
func alignExtra(n int) []byte {
	b := make([]byte, n)
	binary.LittleEndian.PutUint16(b[0:2], alignExtraID)
	binary.LittleEndian.PutUint16(b[2:4], uint16(n-4))
	return b
}

// stripAlignExtra removes padding fields from a copied entry's extra data, so
// that padding is recomputed for the offset this archive puts it at rather than
// inherited from the one it came from.
//
// ⚠ It walks the fields rather than searching for the id: a byte pair equal to
// 0xd935 inside some other field's payload is not a field header.
func stripAlignExtra(extra []byte) []byte {
	out := make([]byte, 0, len(extra))
	for len(extra) >= 4 {
		id := binary.LittleEndian.Uint16(extra[0:2])
		size := int(binary.LittleEndian.Uint16(extra[2:4]))
		if 4+size > len(extra) {
			// A malformed field: everything from here is kept as it was,
			// because a parser that cannot see the end of a field cannot know
			// where the next one starts.
			return append(out, extra...)
		}
		if id != alignExtraID {
			out = append(out, extra[:4+size]...)
		}
		extra = extra[4+size:]
	}
	return append(out, extra...)
}
