// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"bytes"
	"math/rand/v2"
	"path/filepath"
	"testing"
)

// payloadSize is the size of the real payload this packager handles: the
// android/arm64 build of ./cmd/xrcapture measured 2 818 342 bytes on
// 2026-11-07, and xrdesk's was 11 534 630. The smaller one is the default case.
const payloadSize = 2_818_342

// dexSize is what d8 produced for the six Java files of the host on the same
// day, before compression.
const dexSize = 69_648 * 3

// compressible builds bytes that deflate the way a Go binary does, so the
// benchmark is not measuring the best or the worst case of the compressor. A Go
// binary is mostly machine code with long runs of zero padding and repeated
// symbol names; random bytes alone would make Deflate do its worst work and
// make storing look better than it is.
func compressible(n int) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	b := make([]byte, 0, n)
	words := [][]byte{
		[]byte("runtime.mallocgc"), []byte("github.com/go-xrkit/android."),
		bytes.Repeat([]byte{0}, 64), []byte("UVCStream"),
	}
	for len(b) < n {
		switch r.IntN(4) {
		case 0:
			// A run of instruction-like noise.
			chunk := make([]byte, 256)
			for i := range chunk {
				chunk[i] = byte(r.UintN(256))
			}
			b = append(b, chunk...)
		default:
			b = append(b, words[r.IntN(len(words))]...)
		}
	}
	return b[:n]
}

// BenchmarkRepack measures the whole zip this package writes: every entry aapt2
// produced copied without recompression, the dex compressed, and the payload
// stored and page-aligned.
//
// ⭐ STORING THE PAYLOAD IS WHY THIS IS FAST, and the speed is a consequence of
// the correctness fix rather than a separate achievement: the shell path
// deflated 2.8 MB of machine code that then could not be aligned, and paid for
// the compression twice over — once here and once at install, where the
// platform extracted it again.
//
// ⚠ IT IS NOT COMPARED WITH `zip` + `zipalign` AS A RATE. Those two write a
// different artefact: a compressed payload, which is the defect. A table
// showing this package three times faster would be comparing a correct result
// with a wrong one, and the honest comparison is the one in Verify — "OK"
// against "OK - compressed".
//
// ⛔ WHAT WAS MEASURED, AND WHAT WAS NOT, 2026-11-08 on an Apple M4 Max:
//
//	BenchmarkRepack                 1787015 B/op   113 allocs/op
//	BenchmarkRepackDeflatedPayload 11309881 B/op   141 allocs/op
//	BenchmarkInspect                  23880 B/op    34 allocs/op
//
// Storing the payload allocates 1.79 MB where compressing it allocates
// 11.3 MB — six times as much, because add() holds the whole deflated result
// before writing it. That is a fact about the CONTROL rather than about the
// shipping path, where the payload is stored and only the dex is compressed.
//
// ⛔ NO TIME IS QUOTED, DELIBERATELY. The one-minute load average was 9.61 and
// then 13.83 while these ran, and this fleet's rule is not to measure above
// about 8: allocation counts are exact under load and durations are not. The
// ns/op the run printed is in the log and is not evidence. Re-run it on a quiet
// machine before writing a time anywhere.
func BenchmarkRepack(b *testing.B) {
	dir := b.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(b, base)
	adds := []Add{
		{Name: "classes.dex", Data: compressible(dexSize)},
		{Name: "lib/arm64-v8a/libxrapp.so", Data: compressible(payloadSize), Store: true},
	}
	out := filepath.Join(dir, "out.apk")
	b.SetBytes(int64(payloadSize + dexSize))
	b.ReportAllocs()
	for b.Loop() {
		if err := Repack(base, out, adds); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRepackDeflatedPayload is the control: the same work with the payload
// compressed, which is what the shell did. It exists so the cost of the
// decision is a number rather than an assertion — and so that nobody has to
// take "storing is cheaper" on faith.
func BenchmarkRepackDeflatedPayload(b *testing.B) {
	dir := b.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(b, base)
	adds := []Add{
		{Name: "classes.dex", Data: compressible(dexSize)},
		// Store:false — the state that made zipalign answer "OK - compressed".
		{Name: "lib/arm64-v8a/libxrapp.so", Data: compressible(payloadSize)},
	}
	out := filepath.Join(dir, "out.apk")
	b.SetBytes(int64(payloadSize + dexSize))
	b.ReportAllocs()
	for b.Loop() {
		if err := Repack(base, out, adds); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInspect measures reading an APK back, which runs twice in every
// build: once for Verify and once for the entry report.
func BenchmarkInspect(b *testing.B) {
	dir := b.TempDir()
	base := filepath.Join(dir, "base.apk")
	fakeBase(b, base)
	out := filepath.Join(dir, "out.apk")
	if err := Repack(base, out, []Add{
		{Name: "lib/arm64-v8a/libxrapp.so", Data: compressible(payloadSize), Store: true},
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Inspect(out); err != nil {
			b.Fatal(err)
		}
	}
}
