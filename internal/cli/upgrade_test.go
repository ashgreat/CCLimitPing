package cli

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyReleaseChecksum(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "limitping_darwin_arm64.tar.gz")
	data := []byte("reviewed release contents")
	if err := os.WriteFile(asset, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	checksums := filepath.Join(dir, "checksums.txt")
	line := fmt.Sprintf("%x  %s\n", sum, filepath.Base(asset))
	if err := os.WriteFile(checksums, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseChecksum(checksums, asset); err != nil {
		t.Fatalf("verifyReleaseChecksum: %v", err)
	}
	if err := os.WriteFile(asset, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := verifyReleaseChecksum(checksums, asset); err == nil {
		t.Fatal("verifyReleaseChecksum accepted tampered asset")
	}
}

func TestVerifyReleaseChecksumRequiresAssetEntry(t *testing.T) {
	dir := t.TempDir()
	asset := filepath.Join(dir, "asset.tar.gz")
	checksums := filepath.Join(dir, "checksums.txt")
	_ = os.WriteFile(asset, []byte("x"), 0o600)
	_ = os.WriteFile(checksums, []byte("abc  another.tar.gz\n"), 0o600)
	if err := verifyReleaseChecksum(checksums, asset); err == nil {
		t.Fatal("verifyReleaseChecksum accepted a missing entry")
	}
}
