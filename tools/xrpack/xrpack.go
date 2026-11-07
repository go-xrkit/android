// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Options is one APK build.
type Options struct {
	Root        string   // the repository root: host/ and the Go module are under it
	Out         string   // where the products go; default <Root>/host/out
	App         string   // the Go package to build, e.g. "./cmd/xrcapture"
	AppBin      string   // a pre-built binary to package instead of building App
	APKName     string   // the APK's base name, default "xrhost"
	ABI         string   // "arm64-v8a"
	GOARCH      string   // "arm64"
	Keystore    string   // default <Root>/host/debug.keystore
	AndroidHome string   // default $ANDROID_HOME
	JavaHome    string   // default $JAVA_HOME
	Edits       Edits    // what the packaged application calls itself
	Env         []string // extra environment for the Go build
}

// Runner runs one external program. It is an interface so that the whole
// pipeline can be exercised without an Android SDK: the tools are the SDK's
// own, and what this package is responsible for is the order, the arguments and
// the zip.
//
// ⛔⛔ env IS IN THE SIGNATURE BECAUSE LEAVING IT OUT COST A BROKEN BUILD ON THE
// FIRST REAL RUN. The first version of this package assembled GOOS=android into
// a slice, passed it to nothing, and used it only in an error message; `go
// build` then produced a Mach-O binary on this workstation and the packager
// happily reached for it. mustBeELF caught it — "first bytes cffaedfe" — which
// is why that check exists, but the lesson is that an environment a Runner
// cannot carry is an environment that will be dropped.
type Runner interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)
}

// ExecRunner runs programs for real.
type ExecRunner struct{}

// Run executes name in dir with env OVERRIDING this process's, and returns its
// combined output.
func (ExecRunner) Run(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = mergeEnv(os.Environ(), env)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %w\n%s", filepath.Base(name), err, out)
	}
	return out, nil
}

// Result is where the products landed.
type Result struct {
	SDK      SDK
	APK      string
	Manifest string
	Entries  []Entry
}

// Pack builds the host APK.
//
// The SDK's tools do the four jobs only they can do — javac compiles the Java
// host, d8 dexes it, aapt2 compiles and links the resources, apksigner signs —
// and everything between them is here: which SDK, which manifest, which files,
// and the zip.
func Pack(ctx context.Context, o Options, run Runner, log io.Writer) (Result, error) {
	var res Result
	if log == nil {
		log = io.Discard
	}
	o, err := o.withDefaults()
	if err != nil {
		return res, err
	}
	sdk, err := FindSDK(o.AndroidHome)
	if err != nil {
		return res, err
	}
	res.SDK = sdk
	fmt.Fprintf(log, "==> SDK: platform android-%d, build-tools %s\n", sdk.API, sdk.BuildTools)
	// Every SDK tool runs with the JDK and the build-tools directory first on
	// PATH; see toolPath.
	toolEnv := []string{toolPath(o, sdk)}

	if err := os.RemoveAll(o.Out); err != nil {
		return res, err
	}
	libDir := filepath.Join(o.Out, "lib", o.ABI)
	if err := os.MkdirAll(filepath.Join(o.Out, "classes"), 0o755); err != nil {
		return res, err
	}
	if err := os.MkdirAll(libDir, 0o755); err != nil {
		return res, err
	}

	// 1. The payload: the application is an ordinary CGO-free Go binary, under
	// a name the platform will load as a native library.
	lib := filepath.Join(libDir, "libxrapp.so")
	if o.AppBin != "" {
		fmt.Fprintf(log, "==> packaging pre-built %s\n", o.AppBin)
		if err := copyFile(o.AppBin, lib); err != nil {
			return res, fmt.Errorf("packaging %s: %w", o.AppBin, err)
		}
	} else {
		fmt.Fprintf(log, "==> go build (CGO_ENABLED=0 GOOS=android GOARCH=%s) %s\n", o.GOARCH, o.App)
		env := append([]string{"GOWORK=off", "CGO_ENABLED=0", "GOOS=android",
			"GOARCH=" + o.GOARCH, toolPath(o, sdk)}, o.Env...)
		if _, err := run.Run(ctx, o.Root, env, "go", "build",
			"-trimpath", "-ldflags=-s", "-o", lib, o.App); err != nil {
			return res, withEnv(err, env)
		}
	}
	if err := mustBeELF(lib); err != nil {
		return res, err
	}

	// 2. The Java host: javac then d8, each driven by an argument file this
	// code writes, because a command line of every .java path is how a build
	// starts failing on the day a file is added.
	host := filepath.Join(o.Root, "host")
	sources, err := filesUnder(filepath.Join(host, "java"), ".java")
	if err != nil {
		return res, err
	}
	if len(sources) == 0 {
		return res, fmt.Errorf("no .java source under %s: the host is Java and an APK built "+
			"without it would install and then fail to start, with hasCode=\"true\" in its "+
			"manifest and no class behind it", filepath.Join(host, "java"))
	}
	fmt.Fprintf(log, "==> javac (%d source(s))\n", len(sources))
	srcList := filepath.Join(o.Out, "sources.txt")
	if err := os.WriteFile(srcList, []byte(strings.Join(sources, "\n")+"\n"), 0o644); err != nil {
		return res, err
	}
	if _, err := run.Run(ctx, o.Root, toolEnv, filepath.Join(o.JavaHome, "bin", "javac"),
		"-source", "17", "-target", "17", "-nowarn",
		"-classpath", sdk.AndroidJar(),
		"-d", filepath.Join(o.Out, "classes"), "@"+srcList); err != nil {
		return res, err
	}

	classes, err := filesUnder(filepath.Join(o.Out, "classes"), ".class")
	if err != nil {
		return res, err
	}
	if len(classes) == 0 {
		return res, fmt.Errorf("javac wrote no .class file under %s, and reported success: "+
			"d8 would then produce an empty dex and the APK would install",
			filepath.Join(o.Out, "classes"))
	}
	fmt.Fprintf(log, "==> d8 (%d class(es))\n", len(classes))
	clsList := filepath.Join(o.Out, "classes.txt")
	if err := os.WriteFile(clsList, []byte(strings.Join(classes, "\n")+"\n"), 0o644); err != nil {
		return res, err
	}
	if _, err := run.Run(ctx, o.Root, toolEnv, sdk.Tool("d8"),
		"--lib", sdk.AndroidJar(), "--min-api", strconv.Itoa(minAPI),
		"--output", o.Out, "@"+clsList); err != nil {
		return res, err
	}

	// 3. The manifest, substituted into a copy.
	src, err := os.ReadFile(filepath.Join(host, "AndroidManifest.xml"))
	if err != nil {
		return res, err
	}
	rendered, err := Render(src, o.Edits)
	if err != nil {
		return res, err
	}
	res.Manifest = filepath.Join(o.Out, "AndroidManifest.xml")
	if err := os.WriteFile(res.Manifest, rendered, 0o644); err != nil {
		return res, err
	}

	// 4. Resources, which exist for one thing: the USB device filter that makes
	// the system offer this application when a headset is attached.
	link := []string{"link", "-I", sdk.AndroidJar(),
		"--manifest", res.Manifest,
		"--min-sdk-version", strconv.Itoa(minAPI),
		"--target-sdk-version", strconv.Itoa(sdk.API)}
	if _, err := os.Stat(filepath.Join(host, "res")); err == nil {
		fmt.Fprintf(log, "==> aapt2 compile\n")
		resZip := filepath.Join(o.Out, "res.zip")
		if _, err := run.Run(ctx, o.Root, toolEnv, sdk.Tool("aapt2"),
			"compile", "--dir", filepath.Join(host, "res"), "-o", resZip); err != nil {
			return res, err
		}
		link = append(link, resZip)
	}
	base := filepath.Join(o.Out, "base.apk")
	fmt.Fprintf(log, "==> aapt2 link\n")
	if _, err := run.Run(ctx, o.Root, toolEnv, sdk.Tool("aapt2"), append(link, "-o", base)...); err != nil {
		return res, err
	}

	// 5. The zip, written here: the dex compressed, the payload stored and
	// page-aligned. See Repack.
	dex, err := os.ReadFile(filepath.Join(o.Out, "classes.dex"))
	if err != nil {
		return res, fmt.Errorf("d8 reported success but %s is not there: %w",
			filepath.Join(o.Out, "classes.dex"), err)
	}
	payload, err := os.ReadFile(lib)
	if err != nil {
		return res, err
	}
	res.APK = filepath.Join(o.Out, o.APKName+".apk")
	fmt.Fprintf(log, "==> package (dex %d B compressed, payload %d B stored and %d-aligned)\n",
		len(dex), len(payload), PageSize)
	if err := Repack(base, res.APK, []Add{
		{Name: "classes.dex", Data: dex},
		{Name: "lib/" + o.ABI + "/libxrapp.so", Data: payload, Store: true},
	}); err != nil {
		return res, err
	}

	// 6. Signing, with a keystore that outlives the build: a fresh key would
	// change the certificate, and Android refuses to update an installed
	// application whose signature changed
	// (INSTALL_FAILED_UPDATE_INCOMPATIBLE).
	if _, err := os.Stat(o.Keystore); err != nil {
		fmt.Fprintf(log, "==> keytool (new %s)\n", filepath.Base(o.Keystore))
		if _, err := run.Run(ctx, o.Root, toolEnv, filepath.Join(o.JavaHome, "bin", "keytool"),
			"-genkeypair", "-keystore", o.Keystore,
			"-storepass", keystorePass, "-keypass", keystorePass,
			"-alias", "xrhost", "-keyalg", "RSA", "-keysize", "2048",
			"-validity", "10000", "-dname", "CN=go-xrkit android host"); err != nil {
			return res, err
		}
	}
	fmt.Fprintf(log, "==> apksigner\n")
	if _, err := run.Run(ctx, o.Root, toolEnv, sdk.Tool("apksigner"), "sign",
		"--ks", o.Keystore, "--ks-pass", "pass:"+keystorePass,
		"--key-pass", "pass:"+keystorePass,
		"--min-sdk-version", strconv.Itoa(minAPI), res.APK); err != nil {
		return res, err
	}

	// 7. Two checks on the artefact, not on the log. One is this package's own
	// and refuses what zipalign reports as "OK - compressed"; the other is the
	// SDK's, so the verdict does not rest on the code that produced the file.
	if err := Verify(res.APK); err != nil {
		return res, err
	}
	if err := zipalignAgrees(ctx, run, o.Root, toolEnv, sdk, res.APK, log); err != nil {
		return res, err
	}
	res.Entries, err = Inspect(res.APK)
	if err != nil {
		return res, err
	}
	fmt.Fprintf(log, "==> %s\n", res.APK)
	return res, nil
}

const (
	// minAPI is the oldest Android this host runs on. 26 is where
	// MediaProjection, virtual displays and Presentation are all present.
	minAPI = 26
	// keystorePass is the password of the debug keystore, which is the
	// Android-wide convention for one and protects a certificate whose only
	// purpose is to let a diagnostic build replace itself.
	keystorePass = "android"
)

// zipalignAgrees runs the SDK's own alignment check and refuses an answer that
// only LOOKS like a pass.
//
// ⛔⛔ EXIT STATUS 0 WAS NOT ENOUGH. `zipalign -c -P 16 -v 4` exits 0 on an APK
// whose library is compressed, printing "(OK - compressed)" — alignment does
// not apply, and "not applicable" and "aligned" come out as the same word. So
// the output is read: the library's line must say OK, and must NOT say
// compressed.
func zipalignAgrees(ctx context.Context, run Runner, dir string, env []string, sdk SDK, apk string, log io.Writer) error {
	// ⚠ A nil writer is a caller that wants no journal, not a crash. Pack
	// substitutes io.Discard for itself, and this function is reachable
	// without it.
	if log == nil {
		log = io.Discard
	}
	out, err := run.Run(ctx, dir, env, sdk.Tool("zipalign"), "-c", "-P", "16", "-v", "4", apk)
	if err != nil {
		return fmt.Errorf("zipalign refused %s: %w", apk, err)
	}
	var seen int
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, ".so") {
			continue
		}
		seen++
		if strings.Contains(line, "compressed") {
			return fmt.Errorf("zipalign exited 0 but says of the payload: %q — \"OK - "+
				"compressed\" means the alignment check did not apply to this entry",
				strings.TrimSpace(line))
		}
		if !strings.Contains(line, "OK") {
			return fmt.Errorf("zipalign: %q", strings.TrimSpace(line))
		}
	}
	if seen == 0 {
		return fmt.Errorf("zipalign -v listed no .so entry in %s, so its exit status says "+
			"nothing about the payload:\n%s", apk, out)
	}
	fmt.Fprintf(log, "==> zipalign -c -P 16: %d librar(y/ies) examined, stored and aligned\n", seen)
	return nil
}

// withDefaults fills in what the caller left out and refuses what cannot be
// defaulted.
func (o Options) withDefaults() (Options, error) {
	if o.Root == "" {
		return o, fmt.Errorf("no repository root given")
	}
	abs, err := filepath.Abs(o.Root)
	if err != nil {
		return o, err
	}
	o.Root = abs
	if o.Out == "" {
		o.Out = filepath.Join(o.Root, "host", "out")
	}
	if o.App == "" && o.AppBin == "" {
		o.App = "./cmd/xrcapture"
	}
	if o.APKName == "" {
		o.APKName = "xrhost"
	}
	if o.ABI == "" {
		o.ABI = "arm64-v8a"
	}
	if o.GOARCH == "" {
		o.GOARCH = "arm64"
	}
	if o.Keystore == "" {
		o.Keystore = filepath.Join(o.Root, "host", "debug.keystore")
	}
	if o.AndroidHome == "" {
		o.AndroidHome = os.Getenv("ANDROID_HOME")
	}
	if o.JavaHome == "" {
		o.JavaHome = os.Getenv("JAVA_HOME")
		if o.JavaHome == "" {
			return o, fmt.Errorf("JAVA_HOME is not set and no JDK was given; javac and " +
				"keytool come from it")
		}
	}
	if o.Edits.Package == "" {
		o.Edits.Package = "org.goxrkit.androidhost"
	}
	if o.Edits.Label == "" {
		o.Edits.Label = "go-xrkit"
	}
	return o, nil
}

// filesUnder lists every file with the given extension, sorted, so that two
// builds of the same tree hand the compiler the same list in the same order.
func filesUnder(dir, ext string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ext) {
			out = append(out, p)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o755)
}

// mustBeELF refuses a payload that is not an Android executable.
//
// ⛔ A BINARY FOR THE WRONG PLATFORM INSTALLS AND THEN DOES NOTHING. GOOS and
// GOARCH are strings on a command line; `go build` with GOOS unset produces a
// Mach-O on this workstation, and the APK is just as valid with one inside. The
// first four bytes are the measurement.
func mustBeELF(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	var head [4]byte
	if _, err := io.ReadFull(f, head[:]); err != nil {
		return fmt.Errorf("reading the payload %s: %w", path, err)
	}
	if !bytes.Equal(head[:], []byte{0x7f, 'E', 'L', 'F'}) {
		return fmt.Errorf("the payload %s is not an ELF binary (first bytes %x): Android "+
			"loads ELF, so this was built for the wrong platform and would be packaged, "+
			"installed and silently absent", path, head)
	}
	return nil
}

func withEnv(err error, env []string) error {
	return fmt.Errorf("%w\n(environment: %s)", err, strings.Join(env, " "))
}

// mergeEnv returns base with over's variables REPLACING rather than following
// the ones already there.
//
// ⛔⛔ APPENDING IS NOT OVERRIDING, AND THE DIFFERENCE IS PATH. Two entries for
// the same name in an environment are resolved by the C library, which takes
// the FIRST, so appending PATH=… leaves the process's own PATH in force. That
// is how `d8` — a wrapper script that calls `java` from PATH — found this
// workstation's Java 8 and answered
//
//	UnsupportedClassVersionError: com/android/tools/r8/D8 has been compiled by
//	a more recent version of the Java Runtime (class file version 55.0)
//
// while javac, invoked by absolute path, had just compiled the same sources for
// Java 17. The shell this package replaces did `PATH="$JAVA_HOME/bin:$bt:$PATH"`
// and was right to.
func mergeEnv(base, over []string) []string {
	name := func(kv string) string {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			return kv[:i]
		}
		return kv
	}
	replaced := make(map[string]string, len(over))
	for _, kv := range over {
		replaced[name(kv)] = kv
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if _, ok := replaced[name(kv)]; !ok {
			out = append(out, kv)
		}
	}
	for _, kv := range over {
		out = append(out, kv)
	}
	return out
}

// toolPath is the PATH the SDK's tools need: the JDK first, then the
// build-tools directory, then whatever was there.
//
// ⚠ IT IS NOT ENOUGH TO CALL THEM BY ABSOLUTE PATH. d8, apksigner and
// zipalign are wrapper scripts; what they exec is `java`, and which java that
// is comes from PATH.
func toolPath(o Options, sdk SDK) string {
	parts := []string{filepath.Join(o.JavaHome, "bin"), filepath.Dir(sdk.Tool("d8"))}
	if p := os.Getenv("PATH"); p != "" {
		parts = append(parts, p)
	}
	return "PATH=" + strings.Join(parts, string(os.PathListSeparator))
}
