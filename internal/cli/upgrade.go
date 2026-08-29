package cli

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

const (
	releaseDownloadBase = "https://github.com/ashgreat/CCLimitPing/releases/latest/download"
	releaseChecksumName = "checksums.txt"
)

func newUpgradeCmd() *cobra.Command {
	text := localizedText()
	return &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"up", "update"},
		Short:   text.upgradeShort,
		Long:    text.upgradeLong,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			return runUpgrade(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
}

func runUpgrade(ctx context.Context, out, errOut io.Writer) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("self-upgrade is not supported on Windows; download the latest zip from GitHub releases")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolving current executable: %w", err)
	}

	asset, err := releaseAssetName()
	if err != nil {
		return err
	}
	url := releaseDownloadBase + "/" + asset
	fmt.Fprintf(out, "Downloading %s\n", url)

	tmp, err := os.MkdirTemp("", "limitping-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	archivePath := filepath.Join(tmp, asset)
	if err := downloadFile(ctx, url, archivePath); err != nil {
		return err
	}
	checksumPath := filepath.Join(tmp, releaseChecksumName)
	if err := downloadFile(ctx, releaseDownloadBase+"/"+releaseChecksumName, checksumPath); err != nil {
		return fmt.Errorf("downloading release checksums: %w", err)
	}
	if err := verifyReleaseChecksum(checksumPath, archivePath); err != nil {
		return err
	}
	fmt.Fprintln(out, "Verified SHA-256 checksum")
	nextBin := filepath.Join(tmp, "limitping")
	if err := extractLimitping(archivePath, nextBin); err != nil {
		return err
	}
	if err := os.Chmod(nextBin, 0o755); err != nil {
		return err
	}

	if err := replaceExecutable(nextBin, exe, out, errOut); err != nil {
		return err
	}
	fmt.Fprintf(out, "Upgraded limitping -> %s\n", exe)
	cmd := exec.Command(exe, "version")
	cmd.Stdout = out
	cmd.Stderr = errOut
	_ = cmd.Run()
	return nil
}

func verifyReleaseChecksum(checksumsPath, assetPath string) error {
	f, err := os.Open(checksumsPath)
	if err != nil {
		return err
	}
	defer f.Close()
	wanted := filepath.Base(assetPath)
	expected := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && strings.TrimPrefix(fields[1], "*") == wanted {
			expected = strings.ToLower(fields[0])
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if expected == "" {
		return fmt.Errorf("release checksum list has no entry for %s", wanted)
	}

	asset, err := os.Open(assetPath)
	if err != nil {
		return err
	}
	defer asset.Close()
	h := sha256.New()
	if _, err := io.Copy(h, asset); err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", h.Sum(nil))
	if actual != expected {
		return fmt.Errorf("SHA-256 checksum mismatch for %s", wanted)
	}
	return nil
}

func releaseAssetName() (string, error) {
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	switch goos {
	case "darwin", "linux":
		return fmt.Sprintf("limitping_%s_%s.tar.gz", goos, goarch), nil
	case "windows":
		return fmt.Sprintf("limitping_%s_%s.zip", goos, goarch), nil
	default:
		return "", fmt.Errorf("unsupported OS %q (build from source: go build ./cmd/limitping)", goos)
	}
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("download returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return nil
}

func extractLimitping(archivePath, dest string) error {
	switch {
	case strings.HasSuffix(archivePath, ".tar.gz"):
		return extractLimitpingTarGz(archivePath, dest)
	case strings.HasSuffix(archivePath, ".zip"):
		return extractLimitpingZip(archivePath, dest)
	default:
		return fmt.Errorf("unsupported release archive: %s", archivePath)
	}
}

func extractLimitpingTarGz(archivePath, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.FileInfo().IsDir() || filepath.Base(h.Name) != "limitping" {
			continue
		}
		return writeExtractedFile(dest, tr)
	}
	return fmt.Errorf("release archive does not contain limitping")
}

func extractLimitpingZip(archivePath, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Base(f.Name) != "limitping.exe" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		err = writeExtractedFile(dest, rc)
		_ = rc.Close()
		return err
	}
	return fmt.Errorf("release archive does not contain limitping.exe")
}

func writeExtractedFile(dest string, src io.Reader) error {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, src)
	return err
}

func replaceExecutable(src, dest string, out, errOut io.Writer) error {
	replaceErr := atomicReplaceExecutable(src, dest)
	if replaceErr == nil {
		return nil
	}
	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("replacing %s: %w", dest, replaceErr)
	}
	fmt.Fprintf(out, "Cannot replace %s directly (%v); retrying with sudo.\n", dest, replaceErr)
	cmd := exec.Command("sudo", "install", "-m", "0755", src, dest)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo install %s: %w", dest, err)
	}
	return nil
}

func atomicReplaceExecutable(src, dest string) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".limitping-upgrade-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	in, err := os.Open(src)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	defer in.Close()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return err
	}
	cleanup = false
	return nil
}
