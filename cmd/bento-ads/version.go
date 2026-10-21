package main

import (
	"fmt"
	"runtime/debug"
	"strings"
)

// readBuildInfo is indirected so tests can inject a fake debug.BuildInfo
// instead of depending on how the test binary itself was built.
var readBuildInfo = debug.ReadBuildInfo

// adsVersion returns bento-ads's own module version, e.g. "v0.1.0" or a
// pseudo-version for untagged commits -- Go stamps these from git itself
// (no ldflags needed), including the "+dirty" suffix on an uncommitted
// tree. Used standalone in the service "starting" Event Log line (see
// program.go) and as the lead field of versionString() below.
func adsVersion(info *debug.BuildInfo) string {
	if info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return "unknown"
	}
	return info.Main.Version
}

// bentoVersion returns the github.com/warpstreamlabs/bento dependency's
// version: its Replace version when replaced, since a local ../bento
// checkout resolves to "(devel)" rather than a real version and is
// omitted -- mirroring how Bento's own internal/cli/run.go resolves the
// version it prints for itself.
func bentoVersion(info *debug.BuildInfo) string {
	if info == nil {
		return ""
	}
	for _, dep := range info.Deps {
		if dep.Path != "github.com/warpstreamlabs/bento" {
			continue
		}
		v := dep.Version
		if dep.Replace != nil {
			v = dep.Replace.Version
		}
		if v == "" || v == "(devel)" {
			return ""
		}
		return v
	}
	return ""
}

// buildSetting returns the named key from info.Settings, or "" if absent.
func buildSetting(info *debug.BuildInfo, key string) string {
	if info == nil {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == key {
			return s.Value
		}
	}
	return ""
}

// shortRevision truncates a full vcs.revision to git's usual short form.
func shortRevision(info *debug.BuildInfo) string {
	rev := buildSetting(info, "vcs.revision")
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return rev
}

// versionString is bento-ads's own `--version`/`-v` output: a one-line,
// Go-norm summary built entirely from the binary's embedded build info
// (module version, commit, commit time, Go version, and the bento
// dependency's version), not ldflags.
func versionString() string {
	info, ok := readBuildInfo()
	if !ok {
		info = nil
	}

	var detail []string
	if rev := shortRevision(info); rev != "" {
		detail = append(detail, fmt.Sprintf("commit %s", rev))
	}
	if t := buildSetting(info, "vcs.time"); t != "" {
		detail = append(detail, t)
	}
	if bv := bentoVersion(info); bv != "" {
		detail = append(detail, fmt.Sprintf("bento %s", bv))
	}
	goVersion := "unknown"
	if info != nil && info.GoVersion != "" {
		goVersion = info.GoVersion
	}
	detail = append(detail, goVersion)

	return fmt.Sprintf("bento-ads %s (%s)", adsVersion(info), strings.Join(detail, ", "))
}
