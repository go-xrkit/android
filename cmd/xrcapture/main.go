// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

// Command xrcapture is the on-device proof for github.com/go-xrkit/android: an
// ordinary CGO_ENABLED=0 GOOS=android binary that asks the host for consent,
// captures the screen, and reports what it actually got.
//
// It exists because reading documentation is not evidence. It proves the one
// thing a capture can silently fail at — delivering the SAME bytes forever — by
// hashing every frame, and it writes one of them out as a PNG.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

	android "github.com/go-xrkit/android"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("xrcapture: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var (
		seconds = flag.Int("seconds", 12, "how long to capture for")
		outDir  = flag.String("out", ".", "where to write the PNG artefact")
		pngAt   = flag.Int("png-at", 60, "which frame to save as a PNG; late enough that the screen has settled")
		fps     = flag.Float64("fps", 60, "frame-rate ceiling")
	)
	flag.Parse()

	if !android.Available() {
		return fmt.Errorf("no host: set %s or run inside the APK", android.EnvSocket)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*seconds+120)*time.Second)
	defer cancel()

	ds, err := android.Displays(ctx)
	if err != nil {
		return fmt.Errorf("listing displays: %w", err)
	}
	for _, d := range ds {
		log.Printf("DISPLAY %s", d)
	}

	if !android.Authorized() {
		log.Print("asking for screen-capture consent")
		ok, err := android.RequestAuthorization(ctx)
		if err != nil {
			return fmt.Errorf("consent: %w", err)
		}
		if !ok {
			return fmt.Errorf("consent refused")
		}
	}
	log.Print("consent held")

	d, err := android.DefaultDisplay(ctx)
	if err != nil {
		return err
	}
	st, err := android.CaptureDisplay(ctx, d, android.Options{FPS: *fps})
	if err != nil {
		return fmt.Errorf("starting the capture: %w", err)
	}
	defer st.Close()
	log.Printf("STREAM %s", st)

	var (
		frames   int
		same     int
		prev     string
		wrote    bool
		first    time.Time
		last     time.Time
		totalNS  int64
		mem0     runtime.MemStats
		mem1     runtime.MemStats
		deadline = time.Now().Add(time.Duration(*seconds) * time.Second)
	)
	runtime.GC()
	runtime.ReadMemStats(&mem0)

	for time.Now().Before(deadline) {
		wctx, wcancel := context.WithTimeout(ctx, 500*time.Millisecond)
		t0 := time.Now()
		f, err := st.WaitFrame(wctx)
		wcancel()
		if err != nil {
			if serr := st.Err(); serr != nil {
				return fmt.Errorf("the capture ended: %w", serr)
			}
			// A motionless screen legitimately produces nothing.
			continue
		}
		totalNS += time.Since(t0).Nanoseconds()
		if frames == 0 {
			first = time.Now()
			log.Printf("FIRST frame %dx%d stride %d (width*4 = %d, padding %d) seq %d",
				f.Width, f.Height, f.Stride, f.Width*4, f.Stride-f.Width*4, f.Seq)
		}
		last = time.Now()
		frames++

		// The whole point: a capture that hands back a frozen buffer looks
		// exactly like a working one until somebody hashes it.
		sum := sha256.Sum256(f.Pix)
		h := hex.EncodeToString(sum[:8])
		if h == prev {
			same++
		}
		prev = h

		if !wrote && frames >= *pngAt {
			wrote = true
			if err := writePNG(f, filepath.Join(*outDir, "android-capture.png")); err != nil {
				log.Printf("writing the PNG: %v", err)
			}
		}
	}
	runtime.ReadMemStats(&mem1)

	if frames == 0 {
		return fmt.Errorf("no frames arrived at all")
	}
	secs := last.Sub(first).Seconds()
	log.Printf("RESULT frames=%d over %.2fs = %.1f fps; identical-to-previous=%d/%d",
		frames, secs, float64(frames-1)/secs, same, frames-1)
	log.Printf("RESULT %.3f ms per frame in WaitFrame; %.2f allocations per frame",
		float64(totalNS)/float64(frames)/1e6,
		float64(mem1.Mallocs-mem0.Mallocs)/float64(frames))
	log.Printf("RESULT stats %+v", st.Stats())
	if same == frames-1 && frames > 2 {
		return fmt.Errorf("every frame was identical: the capture is frozen")
	}
	hotPath(st)
	return nil
}

// hotPath measures Frame alone, which is what a compositor calls once per
// rendered frame and where the package's no-copy, no-allocation promise lives.
// The loop above cannot say anything about it: its own hashing, its PNG and its
// per-iteration context allocate far more than the call being measured.
func hotPath(st *android.Stream) {
	const n = 200000
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	t0 := time.Now()
	for i := 0; i < n; i++ {
		if f, _ := st.Frame(); len(f.Pix) == 0 {
			log.Print("HOT no frame to borrow; skipping the hot-path measurement")
			return
		}
	}
	d := time.Since(t0)
	runtime.ReadMemStats(&b)
	log.Printf("RESULT Frame(): %.1f ns/op, %.3f allocations/op over %d calls",
		float64(d.Nanoseconds())/n, float64(b.Mallocs-a.Mallocs)/n, n)
}

func writePNG(f android.Frame, path string) error {
	img, err := f.NRGBA()
	if err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := png.Encode(out, img); err != nil {
		return err
	}
	log.Printf("wrote %s (%dx%d)", path, f.Width, f.Height)
	return nil
}
