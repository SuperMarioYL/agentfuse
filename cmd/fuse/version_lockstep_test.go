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
}
