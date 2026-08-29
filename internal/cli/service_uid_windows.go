//go:build windows

package cli

// service commands reject non-macOS platforms before this value is used. The
// stub keeps the shared CLI package cross-compilable for Windows releases.
func currentUID() int { return 0 }
