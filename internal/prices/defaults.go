package prices

import _ "embed"

// bundledTOML is the in-binary snapshot of assets/prices.toml. The file is
// embedded at build time so a fresh `go install` works without copying assets.
//
//go:embed prices_bundled.toml
var bundledTOML []byte

// BundledTOML returns the raw bytes of the bundled price table for tests and
// for callers that want to inspect what shipped with the binary.
func BundledTOML() []byte {
	out := make([]byte, len(bundledTOML))
	copy(out, bundledTOML)
	return out
}
