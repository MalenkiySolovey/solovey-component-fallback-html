// Package fallbackhtml provides the in-process Fallback Sites source descriptor.
package fallbackhtml

import _ "embed"

//go:embed component.json
var manifest []byte

// ManifestJSON returns a copy of the original component contribution descriptor.
func ManifestJSON() []byte { return append([]byte(nil), manifest...) }
