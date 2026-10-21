package main

import (
	"runtime/debug"
	"strings"
	"testing"
)

// fakeBuildInfo builds a *debug.BuildInfo with just the fields versionString
// reads, for the scenario under test.
func fakeBuildInfo(mainVersion, revision, vcsTime, goVersion string, bentoVersion, bentoReplaceVersion string) *debug.BuildInfo {
	info := &debug.BuildInfo{
		GoVersion: goVersion,
		Main:      debug.Module{Version: mainVersion},
	}
	if revision != "" {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.revision", Value: revision})
	}
	if vcsTime != "" {
		info.Settings = append(info.Settings, debug.BuildSetting{Key: "vcs.time", Value: vcsTime})
	}
	if bentoVersion != "" {
		dep := debug.Module{Path: "github.com/warpstreamlabs/bento", Version: bentoVersion}
		if bentoReplaceVersion != "" {
			dep.Replace = &debug.Module{Version: bentoReplaceVersion}
		}
		info.Deps = append(info.Deps, &dep)
	}
	return info
}

// withBuildInfo swaps readBuildInfo for the duration of a test.
func withBuildInfo(t *testing.T, info *debug.BuildInfo, ok bool) {
	t.Helper()
	orig := readBuildInfo
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
	t.Cleanup(func() { readBuildInfo = orig })
}

func TestVersionStringTagged(t *testing.T) {
	withBuildInfo(t, fakeBuildInfo("v0.1.0", "dff0493c1948d424d824cbee263aed5619cb72e4", "2026-10-21T10:29:52Z", "go1.27.2", "v1.21.2", ""), true)

	got := versionString()
	want := "bento-ads v0.1.0 (commit dff0493c1948, 2026-10-21T10:29:52Z, bento v1.21.2, go1.27.2)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVersionStringPseudoVersion(t *testing.T) {
	withBuildInfo(t, fakeBuildInfo("v0.1.1-0.20261021102952-dff0493c1948", "dff0493c1948d424d824cbee263aed5619cb72e4", "2026-10-21T10:29:52Z", "go1.27.2", "v1.21.2", ""), true)

	got := versionString()
	if !strings.HasPrefix(got, "bento-ads v0.1.1-0.20261021102952-dff0493c1948 (") {
		t.Fatalf("got %q, want pseudo-version prefix", got)
	}
}

func TestVersionStringDirty(t *testing.T) {
	// Go itself appends "+dirty" to Main.Version on vcs.modified=true;
	// versionString just passes that through untouched.
	withBuildInfo(t, fakeBuildInfo("v0.1.0+dirty", "dff0493c1948d424d824cbee263aed5619cb72e4", "2026-10-21T10:29:52Z", "go1.27.2", "v1.21.2", ""), true)

	got := versionString()
	if !strings.HasPrefix(got, "bento-ads v0.1.0+dirty (") {
		t.Fatalf("got %q, want a +dirty version", got)
	}
}

func TestVersionStringReplacedBentoUsesReplaceVersion(t *testing.T) {
	withBuildInfo(t, fakeBuildInfo("v0.1.0", "dff0493c1948d424d824cbee263aed5619cb72e4", "2026-10-21T10:29:52Z", "go1.27.2", "v1.21.2", "v1.21.3-replaced"), true)

	got := versionString()
	if !strings.Contains(got, "bento v1.21.3-replaced") {
		t.Fatalf("got %q, want the Replace version, not the required v1.21.2", got)
	}
}

func TestVersionStringLocalReplaceOmitsBento(t *testing.T) {
	// A filesystem replace (replace .../bento => ../bento) resolves to
	// "(devel)", which has no meaningful version to show.
	withBuildInfo(t, fakeBuildInfo("v0.1.0", "dff0493c1948d424d824cbee263aed5619cb72e4", "2026-10-21T10:29:52Z", "go1.27.2", "v1.21.2", "(devel)"), true)

	got := versionString()
	if strings.Contains(got, "bento ") || strings.Contains(got, "bento v") {
		t.Fatalf("got %q, want no bento field for a local (devel) replace", got)
	}
}

func TestVersionStringMissingVCSInfo(t *testing.T) {
	withBuildInfo(t, fakeBuildInfo("(devel)", "", "", "go1.27.2", "", ""), true)

	got := versionString()
	want := "bento-ads unknown (go1.27.2)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestVersionStringNoBuildInfo(t *testing.T) {
	withBuildInfo(t, nil, false)

	got := versionString()
	want := "bento-ads unknown (unknown)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
