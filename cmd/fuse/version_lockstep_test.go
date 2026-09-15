package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionSurfacesLockstep is the v0.13 regression for
// fix-version-drift-v0.12.0-multi-surface. The v0.12.0 release commit
// (ea07155) changed only internal/proxy/anthropic.go + its test and never
// bumped any version surface, so `fuse --version` reported 0.11.0 at the
// v0.12.0 tag (VERSION read 0.11.0, cmd/fuse/main.go's version var read
// "0.11.0", web/site.json's content_version read v0.10.0). This test asserts
// every version surface agrees with the canonical VERSION file, so the next
// bump that touches only one surface is caught at test time. VERSION is the
// single source of truth.
func TestVersionSurfacesLockstep(t *testing.T) {
	// cmd/fuse tests run with cwd = cmd/fuse; the repo root is two levels up.
	root := filepath.Join("..", "..")

	wantBytes, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION file: %v", err)
	}
	want := strings.TrimSpace(string(wantBytes))

	// cmd/fuse/main.go var version must equal VERSION (bare, no leading v).
	// This is the literal `fuse --version` reads under `go build`/`go install`
	// (goreleaser overrides it via ldflags for release binaries, but a source
	// build must still report the honest version).
	if version != want {
		t.Errorf("cmd/fuse main.go version var = %q, want %q (VERSION file); "+
			"`fuse --version` will mis-report under a source build", version, want)
	}

	// web/site.json content_version + the latest changelog entry must equal
	// "v"+VERSION, so the marketing site does not lag a release.
	siteBytes, err := os.ReadFile(filepath.Join(root, "web", "site.json"))
	if err != nil {
		t.Fatalf("read web/site.json: %v", err)
	}
	var site struct {
		ContentVersion string `json:"content_version"`
		Changelog      []struct {
			Version string `json:"version"`
		} `json:"changelog"`
	}
	if err := json.Unmarshal(siteBytes, &site); err != nil {
		t.Fatalf("parse web/site.json: %v", err)
	}
	if site.ContentVersion != "v"+want {
		t.Errorf("web/site.json content_version = %q, want %q",
			site.ContentVersion, "v"+want)
	}
	if len(site.Changelog) == 0 {
		t.Fatal("web/site.json changelog is empty — expected at least one entry")
	}
	if site.Changelog[0].Version != "v"+want {
		t.Errorf("web/site.json changelog[0].version = %q, want %q "+
			"(latest changelog entry lags VERSION)", site.Changelog[0].Version, "v"+want)
	}

	// v0.14: quality-changelog-lockstep-surface — CHANGELOG.md was one of the
	// four drifted surfaces of the v0.12.0 release this test was written to
	// guard, but the original check omitted it: a bump that touches VERSION +
	// main.go + site.json while skipping CHANGELOG.md would pass. The latest
	// `## [x.y.z]` heading must equal VERSION and the [Unreleased] section
	// must stay empty (content there means a shipped change was never given a
	// version section).
	changelogBytes, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	var (
		seenLatest   bool
		inUnreleased bool
	)
	for _, line := range strings.Split(string(changelogBytes), "\n") {
		switch {
		case strings.HasPrefix(line, "## [Unreleased]"):
			inUnreleased = true
		case strings.HasPrefix(line, "## ["):
			inUnreleased = false
			if !seenLatest {
				seenLatest = true
				// Headings carry a date suffix: `## [0.14.0] — 2026-09-16`.
				// Compare the bracketed version only.
				end := strings.IndexByte(line, ']')
				if end < 0 || line[:end+1] != "## ["+want+"]" {
					t.Errorf("CHANGELOG.md latest section = %q, want a heading "+
						"starting \"## [%s]\" (CHANGELOG lags VERSION)", line, want)
				}
			}
		case inUnreleased:
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				t.Errorf("CHANGELOG.md [Unreleased] section is not empty: %q "+
					"(shipped changes must land under a version section)", line)
			}
		}
	}
	if !seenLatest {
		t.Fatal("CHANGELOG.md has no `## [x.y.z]` version section")
	}
}
