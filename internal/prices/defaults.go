package prices

import _ "embed"

// bundledTOML is the in-binary snapshot of assets/prices.toml. The file is
// embedded at build time so a fresh `go install` works without copying assets.
//
//go:embed prices_bundled.toml
var bundledTOML []byte

// SnapshotDate is the date the bundled price table was captured. There is no
// remote refresh by design (the trust premise is "no network"), so this is how
// `fuse prices` tells the user how stale the bundled snapshot is.
const SnapshotDate = "2026-05"

// BundledTOML returns the raw bytes of the bundled price table for tests and
// for callers that want to inspect what shipped with the binary.
func BundledTOML() []byte {
	out := make([]byte, len(bundledTOML))
	copy(out, bundledTOML)
	return out
}
