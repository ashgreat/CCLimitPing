package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/wavever/CCLimitPing/internal/config"
)

func newUninstallCmd() *cobra.Command {
	var keepConfig bool
	text := localizedText()
	cmd := &cobra.Command{
		Use:     "uninstall",
		Aliases: []string{"rm", "remove"},
		Short:   text.uninstallShort,
		Long:    text.uninstallLong,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUninstall(cmd.OutOrStdout(), cmd.ErrOrStderr(), keepConfig)
		},
	}
	cmd.Flags().BoolVar(&keepConfig, "keep-config", false, text.uninstallKeepConfig)
	return cmd
}

func runUninstall(out, errOut io.Writer, keepConfig bool) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating current executable: %w", err)
	}

	// Strip our hook entries from the CLI configs first, so we don't leave hooks
	// pointing at a binary we're about to delete. Best-effort: never abort.
	removeHooksBestEffort(errOut)
	if runtime.GOOS == "darwin" {
		servicePlist, err := servicePlistPath()
		if err != nil {
			fmt.Fprintf(errOut, "warning: locating macOS service: %v\n", err)
		} else if _, err := os.Stat(servicePlist); err == nil {
			if err := runServiceUninstall(out); err != nil {
				fmt.Fprintf(errOut, "warning: removing macOS service: %v\n", err)
			}
		}
	}

	if err := removeExecutable(exe, out, errOut); err != nil {
		return err
	}
	fmt.Fprintf(out, "Removed %s\n", exe)

	if keepConfig {
		fmt.Fprintln(out, "Config/cache preserved.")
		return nil
	}

	dir, err := config.Dir()
	if err != nil {
		return fmt.Errorf("locating config dir: %w", err)
	}
	removed, err := removeConfigDir(dir)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(out, "Removed %s\n", dir)
	} else {
		fmt.Fprintf(out, "No config/cache dir found at %s\n", dir)
	}
	return nil
}

func removeExecutable(path string, out, errOut io.Writer) error {
	if err := os.Remove(path); err == nil {
		return nil
	} else if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("installed binary not found at %s", path)
	}

	if _, err := exec.LookPath("sudo"); err != nil {
		return fmt.Errorf("removing %s: permission denied; retry with sudo", path)
	}
	fmt.Fprintf(out, "Cannot remove %s without elevated permissions; retrying with sudo.\n", path)
	cmd := exec.Command("sudo", "rm", "-f", path)
	cmd.Stdin = os.Stdin
	cmd.Stdout = out
	cmd.Stderr = errOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sudo remove %s: %w", path, err)
	}
	return nil
}

func removeConfigDir(path string) (bool, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("checking config/cache dir %s: %w", path, err)
	}
	if err := os.RemoveAll(path); err != nil {
		return false, fmt.Errorf("removing config/cache dir %s: %w", path, err)
	}
	return true, nil
}
