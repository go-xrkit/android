// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBase writes something shaped like what aapt2 link produces: a stored
// resources.arsc, a stored binary manifest and a compressed resource, with an
// odd-length name so the data offsets are not accidentally aligned.
// It takes a testing.TB rather than a *testing.T so the benchmarks can ask for
// the same archive the tests judge: a benchmark built on a different fixture
// would be measuring something else.
func fakeBase(t testing.TB, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := zip.NewWriter(f)
	for _, e := range []struct {
		name   string
		method uint16
		data   []byte
	}{
		{"AndroidManifest.xml", zip.Store, []byte("binary xml, five words long")},
		{"resources.arsc", zip.Store, bytes.Repeat([]byte("r"), 1031)},
		{"res/xml/device_filter.xml", zip.Deflate, bytes.Repeat([]byte("x"), 4096)},
	} {
		hw, err := w.CreateHeader(&zip.FileHeader{Name: e.name, Method: e.method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hw.Write(e.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// entries indexes an APK by name.
func entries(t *testing.T, apk string) map[string]Entry {
	t.Helper()
	es, err := Inspect(apk)
	if err != nil {
		t.Fatal(err)
	}
	m := make(map[string]Entry, len(es))
	for _, e := range es {
		m[e.Name] = e
	}
	return m
}

// TestThePayloadIsStoredAndPageAligned is the measurement this package exists
// to change.
//
// ⛔⛔ BEFORE: `zip -q base.apk classes.dex lib/arm64-v8a/libxrapp.so` deflated
// both, so `zipalign -c -P 16 -v 4` printed
//
//	35831 lib/arm64-v8a/libxrapp.so (OK - compressed)
//	Verification succesful
//
// and exited 0 — on this repository's own APK, build-tools 35.0.0, 2026-11-07.
// Alignment does not apply to a compressed entry, so the check had no subject
// and said OK anyway.
func TestThePayloadIsStoredAndPageAligned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	out := filepath.Join(dir, "xrhost.apk")

	payload := bytes.Repeat([]byte{0x7f, 'E', 'L', 'F'}, 9000)
	dex := bytes.Repeat([]byte("dex\n"), 500)
	if err := Repack(base, out, []Add{
		{Name: "classes.dex", Data: dex},
		{Name: "lib/arm64-v8a/libxrapp.so", Data: payload, Store: true},
	}); err != nil {
		t.Fatal(err)
	}

	got := entries(t, out)
	lib, ok := got["lib/arm64-v8a/libxrapp.so"]
	if !ok {
		t.Fatal("the payload is not in the APK")
	}
	if !lib.Stored() {
		t.Error("the payload is compressed, which is the state zipalign reports as " +
			"\"OK - compressed\" and no alignment check can see past")
	}
	if lib.DataOffset%PageSize != 0 {
		t.Errorf("the payload starts at %d, which is %d past a %d boundary",
			lib.DataOffset, lib.DataOffset%PageSize, PageSize)
	}
	// ⛔ AND THE BYTES MUST BE THE BYTES. An alignment reached by truncating or
	// padding the DATA would satisfy the arithmetic above and ship a library
	// the loader refuses.
	if err := Verify(out); err != nil {
		t.Errorf("Verify refused the APK this package just wrote: %v", err)
	}
	readBack(t, out, "lib/arm64-v8a/libxrapp.so", payload)
	readBack(t, out, "classes.dex", dex)

	// The dex is compressed: it is read by the runtime through the normal
	// reader, so there is nothing to gain from storing a megabyte unpacked.
	if d := got["classes.dex"]; d.Stored() {
		t.Error("classes.dex is stored; it should be deflated")
	}
	// Every stored entry aapt2 wrote is word-aligned, which is what zipalign's
	// positional argument was for.
	for _, name := range []string{"AndroidManifest.xml", "resources.arsc"} {
		e := got[name]
		if !e.Stored() {
			t.Fatalf("%s arrived compressed, so this case is not measuring alignment", name)
		}
		if !e.Aligned(wordAlign) {
			t.Errorf("%s starts at %d, not a multiple of %d", name, e.DataOffset, wordAlign)
		}
	}
	// And the compressed entry is left exactly as aapt2 compressed it.
	if e := got["res/xml/device_filter.xml"]; e.Stored() {
		t.Error("a compressed resource was recompressed as stored")
	}
}

func readBack(t *testing.T, apk, name string, want []byte) {
	t.Helper()
	r, err := zip.OpenReader(apk)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	for _, f := range r.File {
		if f.Name != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rc.Close() }()
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s came back %d bytes, want %d, and %v byte-equal",
				name, len(got), len(want), bytes.Equal(got, want))
		}
		return
	}
	t.Fatalf("%s is not in %s", name, apk)
}

// TestAlignmentHoldsForEveryNameLength walks the one quantity that moves the
// data offset without changing anything else, because the padding is computed
// from 30 + len(name) + len(extra) and an off-by-one there is invisible at one
// particular length.
func TestAlignmentHoldsForEveryNameLength(t *testing.T) {
	t.Parallel()
	for n := 1; n <= 40; n++ {
		dir := t.TempDir()
		base := filepath.Join(dir, "base.apk")
		fakeBase(t, base)
		out := filepath.Join(dir, "out.apk")
		name := "lib/arm64-v8a/" + strings.Repeat("a", n) + ".so"
		if err := Repack(base, out, []Add{
			{Name: name, Data: []byte("payload"), Store: true},
		}); err != nil {
			t.Fatalf("name length %d: %v", n, err)
		}
		e := entries(t, out)[name]
		if e.DataOffset%PageSize != 0 {
			t.Errorf("name length %d: data at %d, %d past a boundary",
				n, e.DataOffset, e.DataOffset%PageSize)
		}
	}
}

// TestTwoLibrariesAreBothAligned: the second one's offset depends on the
// first's padding, so one aligned library is not evidence about two.
func TestTwoLibrariesAreBothAligned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	out := filepath.Join(dir, "out.apk")
	adds := []Add{
		{Name: "lib/arm64-v8a/libxrapp.so", Data: bytes.Repeat([]byte("a"), 100), Store: true},
		{Name: "lib/arm64-v8a/libhelper.so", Data: bytes.Repeat([]byte("b"), 17), Store: true},
	}
	if err := Repack(base, out, adds); err != nil {
		t.Fatal(err)
	}
	got := entries(t, out)
	for _, a := range adds {
		if e := got[a.Name]; e.DataOffset%PageSize != 0 {
			t.Errorf("%s at %d, %d past a boundary", a.Name, e.DataOffset, e.DataOffset%PageSize)
		}
	}
}

// TestASecondEntryOfTheSameNameIsRefused. A zip may hold two, readers disagree
// about which one wins, and the platform's choice is not the one this code
// made.
func TestASecondEntryOfTheSameNameIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	err := Repack(base, filepath.Join(dir, "out.apk"), []Add{
		{Name: "resources.arsc", Data: []byte("mine"), Store: true},
	})
	if err == nil {
		t.Fatal("a name aapt2 had already packaged was spliced in a second time")
	}
	if !strings.Contains(err.Error(), "already in") {
		t.Errorf("refusal %q does not say the name is taken", err)
	}
	// The same refusal applies between two adds.
	err = Repack(base, filepath.Join(dir, "out2.apk"), []Add{
		{Name: "classes.dex", Data: []byte("a")},
		{Name: "classes.dex", Data: []byte("b")},
	})
	if err == nil {
		t.Error("two adds of the same name were accepted")
	}
}

func TestRepackRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := Repack(filepath.Join(dir, "absent.apk"), filepath.Join(dir, "o.apk"), nil); err == nil {
		t.Error("a missing base APK was accepted")
	}
	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	if err := Repack(base, filepath.Join(dir, "no-such-dir", "o.apk"), nil); err == nil {
		t.Error("an unwritable destination was accepted")
	}
}

// TestPadding is the arithmetic on its own.
//
// ⚠ The 1-to-3 cases are the interesting ones: an extra field cannot be shorter
// than its own four-byte header, so the shortfall is paid for with a whole
// further alignment rather than with the bytes asked for.
func TestPadding(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		head int64
		want int
		pad  int
	}{
		{head: 0, want: 4, pad: 0},
		{head: 4, want: 4, pad: 0},
		{head: 1, want: 4, pad: 7},  // 3 needed, but 3 < 4: one more alignment
		{head: 2, want: 4, pad: 6},  // 2 needed
		{head: 3, want: 4, pad: 5},  // 1 needed
		{head: 60, want: 4, pad: 0}, //
		{head: 16384, want: PageSize, pad: 0},
		{head: 16380, want: PageSize, pad: 4},
		{head: 16383, want: PageSize, pad: 1 + PageSize},
		{head: 30, want: 1, pad: 0}, // alignment of 1 asks for nothing
		{head: 30, want: 0, pad: 0},
	} {
		if got := padding(c.head, c.want); got != c.pad {
			t.Errorf("padding(%d, %d) = %d, want %d", c.head, c.want, got, c.pad)
		}
		// And the property, rather than the table: head+pad is on a boundary.
		if c.want > 1 {
			if got := padding(c.head, c.want); (c.head+int64(got))%int64(c.want) != 0 {
				t.Errorf("padding(%d, %d) = %d leaves the data off a boundary",
					c.head, c.want, got)
			}
		}
	}
}

func TestAlignmentForNamesTheLibrary(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		want int
	}{
		{"lib/arm64-v8a/libxrapp.so", PageSize},
		{"classes.dex", wordAlign},
		{"resources.arsc", wordAlign},
		{"res/raw/not-a.so.txt", wordAlign},
	} {
		if got := alignmentFor(c.name); got != c.want {
			t.Errorf("alignmentFor(%q) = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestStripAlignExtraWalksTheFieldsRatherThanSearchingForBytes: a byte pair
// equal to the padding id inside another field's payload is not a field header,
// and treating it as one would cut a valid field in half.
func TestStripAlignExtraWalksTheFieldsRatherThanSearchingForBytes(t *testing.T) {
	t.Parallel()
	field := func(id uint16, payload []byte) []byte {
		b := make([]byte, 4+len(payload))
		binary.LittleEndian.PutUint16(b[0:2], id)
		binary.LittleEndian.PutUint16(b[2:4], uint16(len(payload)))
		copy(b[4:], payload)
		return b
	}
	// A timestamp field whose payload happens to contain 0xd935.
	bait := field(0x5455, []byte{0x01, 0x35, 0xd9, 0x00, 0x00})
	pad := field(alignExtraID, make([]byte, 12))

	got := stripAlignExtra(append(append([]byte{}, bait...), pad...))
	if !bytes.Equal(got, bait) {
		t.Errorf("stripping padding gave %x, want the timestamp field %x", got, bait)
	}
	got = stripAlignExtra(append(append([]byte{}, pad...), bait...))
	if !bytes.Equal(got, bait) {
		t.Errorf("padding first: got %x, want %x", got, bait)
	}
	if got := stripAlignExtra(nil); len(got) != 0 {
		t.Errorf("stripping nothing gave %x", got)
	}
	// A malformed field is kept rather than parsed past: the end of one field
	// is where the next begins, so a length that overruns means the rest
	// cannot be read at all.
	bad := field(0x5455, []byte{1, 2, 3})[:5]
	if got := stripAlignExtra(bad); !bytes.Equal(got, bad) {
		t.Errorf("a truncated field gave %x, want it kept as %x", got, bad)
	}
}

func TestAlignExtraIsAFieldOfExactlyTheLengthAskedFor(t *testing.T) {
	t.Parallel()
	for _, n := range []int{4, 5, 12, 4096, 16388} {
		b := alignExtra(n)
		if len(b) != n {
			t.Fatalf("alignExtra(%d) is %d bytes", n, len(b))
		}
		if id := binary.LittleEndian.Uint16(b[0:2]); id != alignExtraID {
			t.Errorf("alignExtra(%d) carries id %#x, want %#x", n, id, alignExtraID)
		}
		if size := int(binary.LittleEndian.Uint16(b[2:4])); size != n-4 {
			t.Errorf("alignExtra(%d) declares %d payload bytes, want %d", n, size, n-4)
		}
	}
}

// TestRepackIsReproducible: two runs over the same inputs differ in no byte, so
// a rebuild that changes the APK is a change in the sources.
func TestRepackIsReproducible(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(t, base)
	adds := []Add{
		{Name: "classes.dex", Data: []byte("dex")},
		{Name: "lib/arm64-v8a/libxrapp.so", Data: []byte("elf"), Store: true},
	}
	var sums [2][]byte
	for i := range sums {
		out := filepath.Join(dir, "out.apk")
		if err := Repack(base, out, adds); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		sums[i] = b
	}
	if !bytes.Equal(sums[0], sums[1]) {
		t.Errorf("two identical repacks produced %d and %d bytes that differ",
			len(sums[0]), len(sums[1]))
	}
}
