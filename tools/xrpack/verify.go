// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"archive/zip"
	"fmt"
	"strings"
	"time"
)

// zeroTime is the zero time.Time, which tells archive/zip to write the DOS
// timestamp fields verbatim and append no extended-timestamp extra field.
var zeroTime time.Time

// Entry is what Verify and the inspector report about one APK entry.
type Entry struct {
	Name       string
	Method     uint16
	DataOffset int64
	Size       int64
}

// Stored reports whether the entry is uncompressed.
func (e Entry) Stored() bool { return e.Method == zip.Store }

// Aligned reports whether the entry's data begins on a multiple of want.
func (e Entry) Aligned(want int) bool { return want <= 1 || e.DataOffset%int64(want) == 0 }

// Inspect reads an APK and reports every entry's method and data offset. It
// reads the archive rather than the build log, because what matters is where
// the bytes ended up.
func Inspect(apk string) ([]Entry, error) {
	r, err := zip.OpenReader(apk)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", apk, err)
	}
	defer func() { _ = r.Close() }()
	out := make([]Entry, 0, len(r.File))
	for _, f := range r.File {
		off, err := f.DataOffset()
		if err != nil {
			return nil, fmt.Errorf("%s: finding where its data starts: %w", f.Name, err)
		}
		out = append(out, Entry{
			Name:       f.Name,
			Method:     f.Method,
			DataOffset: off,
			Size:       int64(f.UncompressedSize64),
		})
	}
	return out, nil
}

// Verify refuses an APK whose native libraries are not stored uncompressed and
// page-aligned, and whose other stored entries are not word-aligned.
//
// ⛔⛔ IT REFUSES A COMPRESSED .so RATHER THAN PASSING IT. That is the whole
// reason this function exists: `zipalign -c -P 16 -v 4` printed
//
//	35831 lib/arm64-v8a/libxrapp.so (OK - compressed)
//	Verification succesful
//
// about this repository's own APK on 2026-11-07. "OK" there means "alignment
// does not apply to this entry", and the exit status was 0 — a gate whose
// subject had quietly left the room. A check that cannot distinguish "aligned"
// from "not applicable" reports the same thing for both.
func Verify(apk string) error {
	entries, err := Inspect(apk)
	if err != nil {
		return err
	}
	var libs int
	var bad []string
	for _, e := range entries {
		isLib := strings.HasSuffix(e.Name, ".so")
		if isLib {
			libs++
			if !e.Stored() {
				bad = append(bad, fmt.Sprintf("%s is COMPRESSED, so it cannot be mapped from "+
					"the APK and no alignment check applies to it — this is the state "+
					"zipalign reports as \"OK - compressed\"", e.Name))
				continue
			}
			if !e.Aligned(PageSize) {
				bad = append(bad, fmt.Sprintf("%s starts at %d, which is not a multiple of "+
					"%d: a device with 16 KiB pages cannot map it", e.Name, e.DataOffset, PageSize))
			}
			continue
		}
		if e.Stored() && !e.Aligned(wordAlign) {
			bad = append(bad, fmt.Sprintf("%s is stored and starts at %d, which is not a "+
				"multiple of %d", e.Name, e.DataOffset, wordAlign))
		}
	}
	// ⛔⛔ AND AN APK WITH NO LIBRARY IN IT MUST NOT PASS THIS. Every refusal
	// above is about an entry that is present; zero entries would satisfy all
	// of them, which is how a conformance claim becomes vacuous.
	if libs == 0 {
		return fmt.Errorf("%s holds no .so entry at all (%d entries): the payload is the "+
			"application, so an APK without one is not a pass but an empty subject", apk, len(entries))
	}
	if len(bad) > 0 {
		return fmt.Errorf("%s: %s", apk, strings.Join(bad, "; "))
	}
	return nil
}
