// Copyright (c) the go-xrkit/android authors. All rights reserved.
//
// SPDX-License-Identifier: BSD-3-Clause

package xrpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// theManifest is the one this repository ships, read rather than mocked: a
// substitution that works on a fixture and not on the real file is the exact
// failure these markers exist to catch.
func theManifest(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "host", "AndroidManifest.xml"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRenderSubstitutesTheFourThingsAnApplicationMayChange(t *testing.T) {
	t.Parallel()
	out, err := Render(theManifest(t), Edits{
		Package:     "org.goxrkit.xrdesk",
		Label:       "xrdesk",
		Args:        "-head -bend 1.0",
		Permissions: []string{"android.permission.CAMERA", "android.permission.INTERNET"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		`package="org.goxrkit.xrdesk"`,
		`android:label="xrdesk"`,
		`android:name="org.goxrkit.args" android:value="-head -bend 1.0"`,
		`<uses-permission android:name="android.permission.CAMERA" />`,
		`<uses-permission android:name="android.permission.INTERNET" />`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the rendered manifest is missing %s", want)
		}
	}
	// ⛔ AND NO MARKER SURVIVES. One left behind would be substituted a second
	// time if this output were ever fed back in, which is what an APK built
	// from host/out/AndroidManifest.xml would do.
	for _, marker := range []string{markerPackage, markerLabel, markerArgs, markerPermissions} {
		if strings.Contains(s, marker) {
			t.Errorf("marker %s is still in the rendered manifest", marker)
		}
	}
	// The host's own permissions are untouched: the extras are added, not
	// substituted for them.
	if !strings.Contains(s, "android.permission.FOREGROUND_SERVICE_MEDIA_PROJECTION") {
		t.Error("the host's own permissions did not survive the substitution")
	}
}

// TestALabelIsEscapedRatherThanSpliced.
//
// ⛔⛔ THIS IS WHERE THE SHELL WAS WRONG, NOT MERELY CRUDE. In a sed replacement
// an unescaped & stands for THE WHOLE MATCH, so
//
//	sed -e "s|android:label=\"go-xrkit\"|android:label=\"A&B\"|"
//
// produced android:label="Aandroid:label="go-xrkit"B" — valid-looking text that
// aapt2 then read as a different attribute. A double quote closed the attribute
// outright. Both are below as cases, with the expected XML entity.
func TestALabelIsEscapedRatherThanSpliced(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		label string
		want  string
	}{
		{"an ampersand", "A&B", `android:label="A&amp;B"`},
		{"a double quote", `say "hi"`, `android:label="say &#34;hi&#34;"`},
		{"a less-than", "a<b", `android:label="a&lt;b"`},
		{"plain text is left alone", "xrdesk", `android:label="xrdesk"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			out, err := Render(theManifest(t), Edits{
				Package: "org.goxrkit.test", Label: c.label,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), c.want) {
				t.Errorf("label %q rendered without %s", c.label, c.want)
			}
			// And the attribute it replaced is gone exactly once over: the
			// string "go-xrkit" itself stays, in the copyright header, so the
			// check is on the ATTRIBUTE rather than on the word.
			if n := strings.Count(string(out), markerLabel); n != 0 {
				t.Errorf("the replaced label attribute appears %d time(s) in the output", n)
			}
			if n := strings.Count(string(out), "android:label="); n != 1 {
				t.Errorf("the output holds %d label attributes, want 1", n)
			}
		})
	}
}

// TestArgumentsAreEscapedToo, because they come from a command line and reach
// an attribute by the same road.
func TestArgumentsAreEscapedToo(t *testing.T) {
	t.Parallel()
	out, err := Render(theManifest(t), Edits{
		Package: "org.goxrkit.test",
		Args:    `-title "a & b"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `android:value="-title &#34;a &amp; b&#34;"`) {
		t.Error("the argument string was not escaped into the attribute")
	}
}

// TestASubstitutionThatMatchesNothingIsARefusal.
//
// ⛔⛔ THE SHELL'S VERSION SUCCEEDED HERE. `sed` prints its input unchanged when
// the pattern is absent and exits 0, so renaming the label in the manifest
// would have left every packaged application wearing the demo's name, with a
// green build and no message anywhere.
func TestASubstitutionThatMatchesNothingIsARefusal(t *testing.T) {
	t.Parallel()
	edited := strings.Replace(string(theManifest(t)), markerLabel,
		`android:label="something else"`, 1)
	_, err := Render([]byte(edited), Edits{Package: "org.goxrkit.test", Label: "x"})
	if err == nil {
		t.Fatal("a manifest with no label marker was accepted")
	}
	for _, want := range []string{"the label", "holds 0 copies", "want exactly 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not carry %q", err, want)
		}
	}
}

// TestASubstitutionThatMatchesTwiceIsARefusalToo: sed -e would have replaced
// only the first, leaving the second behind and the two disagreeing.
func TestASubstitutionThatMatchesTwiceIsARefusalToo(t *testing.T) {
	t.Parallel()
	doubled := strings.Replace(string(theManifest(t)), markerLabel,
		markerLabel+" "+markerLabel, 1)
	_, err := Render([]byte(doubled), Edits{Package: "org.goxrkit.test", Label: "x"})
	if err == nil || !strings.Contains(err.Error(), "holds 2 copies") {
		t.Errorf("a manifest with two label markers gave %v, want a refusal", err)
	}
}

// TestAPermissionNameIsCheckedRatherThanEscaped. A permission is not free text:
// one holding a quote would be declaring something else, and escaping it would
// declare a permission that cannot exist. Refusing says which of the two the
// caller meant.
func TestAPermissionNameIsCheckedRatherThanEscaped(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		perms []string
		ok    bool
	}{
		{"an ordinary permission", []string{"android.permission.CAMERA"}, true},
		{"underscores and digits", []string{"com.example.perm_2"}, true},
		{"a quote", []string{`android.permission.CAMERA" android:required="true`}, false},
		{"a space", []string{"android.permission.CAMERA android.permission.INTERNET"}, false},
		{"no dot", []string{"CAMERA"}, false},
		{"an empty component", []string{"android..CAMERA"}, false},
		{"a leading digit in a component", []string{"android.2permission"}, false},
		{"markup", []string{"a.<script>"}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Render(theManifest(t), Edits{
				Package: "org.goxrkit.test", Permissions: c.perms,
			})
			if c.ok && err != nil {
				t.Fatalf("%v was refused: %v", c.perms, err)
			}
			if !c.ok && err == nil {
				t.Fatalf("%v was accepted", c.perms)
			}
		})
	}
}

func TestThePackageIdMustBeADottedIdentifier(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		id string
		ok bool
	}{
		{"org.goxrkit.androidhost", true},
		{"a.b", true},
		{"singleword", false},
		{"", false},
		{"a..b", false},
		{"a.b c", false},
		{`a.b" x="y`, false},
		{"1a.b", false},
	} {
		_, err := Render(theManifest(t), Edits{Package: c.id})
		if (err == nil) != c.ok {
			t.Errorf("package id %q: error = %v, want ok = %v", c.id, err, c.ok)
		}
	}
}

// TestNoPermissionsLeavesNoMarkerAndNoEmptyLine: least privilege is the default,
// and the comment that replaces the marker says so in the built manifest.
func TestNoPermissionsLeavesNoMarkerAndNoEmptyLine(t *testing.T) {
	t.Parallel()
	out, err := Render(theManifest(t), Edits{Package: "org.goxrkit.test"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, markerPermissions) {
		t.Error("the permissions marker survived")
	}
	if !strings.Contains(s, "least privilege") {
		t.Error("nothing says why there are no extra permissions")
	}
	if strings.Contains(s, "<uses-permission android:name=\"\"") {
		t.Error("an empty permission element was rendered")
	}
}

// TestAttrSurvivesEveryByte is the escaper on its own: it is the only thing
// between a command line and markup.
func TestAttrSurvivesEveryByte(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"&", "&amp;"},
		{"<>", "&lt;&gt;"},
		{`"`, "&#34;"},
		{"'", "&#39;"},
		{"\n", "&#xA;"},
	} {
		if got := attr(c.in); got != c.want {
			t.Errorf("attr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
