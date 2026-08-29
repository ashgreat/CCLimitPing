package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wavever/CCLimitPing/internal/config"
)

const (
	serviceLabel       = "com.ashgreat.limitping"
	servicePlistName   = serviceLabel + ".plist"
	serviceLogName     = "service.log"
	serviceErrorName   = "service.error.log"
	serviceStopTimeout = 5 * time.Second
	serviceHistoryAge  = 7 * 24 * time.Hour
)

func newServiceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the persistent macOS launchd service",
		Long: "Install a per-user macOS LaunchAgent that starts at login, restarts after failure, " +
			"and can prevent idle system sleep while connected to AC power.",
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newServiceInstallCmd(), newServiceStatusCmd(), newServiceUninstallCmd())
	return cmd
}

func newServiceInstallCmd() *cobra.Command {
	preventSleep := true
	cmd := &cobra.Command{
		Use:       "install [provider]",
		Short:     "Install and start the macOS LaunchAgent",
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"claude", "codex", "spark", "all"},
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := "claude"
			if len(args) == 1 {
				provider = args[0]
			}
			return runServiceInstall(cmd.OutOrStdout(), provider, preventSleep)
		},
	}
	cmd.Flags().BoolVar(&preventSleep, "prevent-sleep", true,
		"use caffeinate -s to prevent idle system sleep while on AC power")
	return cmd
}

func newServiceStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the macOS LaunchAgent is installed and loaded",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServiceStatus(cmd.OutOrStdout())
		},
	}
}

func newServiceUninstallCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "uninstall",
		Aliases: []string{"remove", "rm"},
		Short:   "Stop and remove the macOS LaunchAgent",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runServiceUninstall(cmd.OutOrStdout())
		},
	}
}

func requireDarwinService() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("limitping service is available only on macOS")
	}
	return nil
}

func servicePlistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", servicePlistName), nil
}

func serviceDomain() string {
	return "gui/" + strconv.Itoa(currentUID())
}

func serviceTarget() string {
	return serviceDomain() + "/" + serviceLabel
}

func runServiceInstall(out io.Writer, provider string, preventSleep bool) error {
	if err := requireDarwinService(); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if _, err := selectTargets(cfg, provider); err != nil {
		return err
	}

	// A detached watcher and a LaunchAgent would compete for the same lock and
	// make service state confusing. Require an explicit stop before migration.
	if st, ok := readBgState(); ok && processAlive(st.PID) {
		return fmt.Errorf("background watch is running (pid %d); stop it first with `limitping bg stop`", st.PID)
	}
	wasLoaded := serviceLoaded()
	if st, ok := activeWatchLock(); ok && !wasLoaded {
		return watchAlreadyRunningError(st)
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating limitping binary: %w", err)
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return fmt.Errorf("resolving limitping binary: %w", err)
	}
	configDir, err := config.Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return err
	}
	plistPath, err := servicePlistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return err
	}

	plist := buildServicePlist(servicePlistOptions{
		Executable:   exe,
		Provider:     provider,
		PreventSleep: preventSleep,
		Path:         os.Getenv("PATH"),
		Home:         homeOrEmpty(),
		StdoutPath:   filepath.Join(configDir, serviceLogName),
		StderrPath:   filepath.Join(configDir, serviceErrorName),
	})
	if err := writeFileAtomic(plistPath, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", plistPath, err)
	}

	// Replace an already-loaded copy so updated arguments take effect.
	_ = exec.Command("launchctl", "bootout", serviceDomain(), plistPath).Run()
	deadline := time.Now().Add(serviceStopTimeout)
	for time.Now().Before(deadline) {
		_, watchRunning := activeWatchLock()
		if !serviceLoaded() && !watchRunning {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, watchRunning := activeWatchLock(); serviceLoaded() || watchRunning {
		return fmt.Errorf("previous watcher did not stop within %s", serviceStopTimeout)
	}
	cmd := exec.Command("launchctl", "bootstrap", serviceDomain(), plistPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("loading LaunchAgent: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.Command("launchctl", "kickstart", "-k", serviceTarget()).CombinedOutput(); err != nil {
		return fmt.Errorf("starting LaunchAgent: %w: %s", err, strings.TrimSpace(string(output)))
	}

	fmt.Fprintf(out, "Installed and started %s for %s.\n", serviceLabel, provider)
	fmt.Fprintf(out, "LaunchAgent: %s\n", plistPath)
	fmt.Fprintf(out, "Logs: %s and %s\n", filepath.Join(configDir, serviceLogName), filepath.Join(configDir, serviceErrorName))
	if preventSleep {
		fmt.Fprintln(out, "Idle system sleep will be prevented while the Mac is on AC power and the service is running.")
	}
	return nil
}

func runServiceStatus(out io.Writer) error {
	if err := requireDarwinService(); err != nil {
		return err
	}
	plistPath, err := servicePlistPath()
	if err != nil {
		return err
	}
	_, statErr := os.Stat(plistPath)
	fmt.Fprintf(out, "LaunchAgent file: %s (%s)\n", plistPath, presentStatus(statErr))
	if serviceLoaded() {
		fmt.Fprintf(out, "Service: loaded (%s)\n", serviceTarget())
	} else {
		fmt.Fprintln(out, "Service: not loaded")
	}
	configDir, _ := config.Dir()
	logPath := filepath.Join(configDir, serviceLogName)
	fmt.Fprintf(out, "Logs: %s and %s\n", logPath, filepath.Join(configDir, serviceErrorName))
	if st, ok := activeWatchLock(); ok {
		fmt.Fprintf(out, "Watcher: pid %d, provider %s, started %s\n", st.PID, st.Provider, st.StartedAt.Local().Format("2006-01-02 15:04:05"))
	}
	history, err := readBgPingHistory(logPath, time.Now().Add(-serviceHistoryAge))
	if err == nil && history.total() > 0 {
		last := history.Attempts[len(history.Attempts)-1]
		fmt.Fprintf(out, "Pings in last 7 days: %d (%d succeeded, %d failed, %d dry-run)\n",
			history.total(), history.Succeeded, history.Failed, history.DryRun)
		fmt.Fprintf(out, "Last ping: %s, %s, %s\n", last.At.Local().Format("2006-01-02 15:04:05"), last.Provider, last.Status)
	} else if err == nil {
		fmt.Fprintln(out, "Pings in last 7 days: none recorded")
	} else {
		fmt.Fprintf(out, "Ping history: unavailable (%v)\n", err)
	}
	return nil
}

func runServiceUninstall(out io.Writer) error {
	if err := requireDarwinService(); err != nil {
		return err
	}
	plistPath, err := servicePlistPath()
	if err != nil {
		return err
	}
	_ = exec.Command("launchctl", "bootout", serviceDomain(), plistPath).Run()
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintf(out, "Stopped and removed %s. Configuration and logs were preserved.\n", serviceLabel)
	return nil
}

func serviceLoaded() bool {
	return exec.Command("launchctl", "print", serviceTarget()).Run() == nil
}

func presentStatus(err error) string {
	switch {
	case err == nil:
		return "present"
	case os.IsNotExist(err):
		return "missing"
	default:
		return "unreadable"
	}
}

func homeOrEmpty() string {
	home, _ := os.UserHomeDir()
	return home
}

type servicePlistOptions struct {
	Executable   string
	Provider     string
	PreventSleep bool
	Path         string
	Home         string
	StdoutPath   string
	StderrPath   string
}

func buildServicePlist(o servicePlistOptions) string {
	args := []string{o.Executable, "watch", o.Provider}
	if o.PreventSleep {
		args = append([]string{"/usr/bin/caffeinate", "-s"}, args...)
	}
	var argXML strings.Builder
	for _, arg := range args {
		fmt.Fprintf(&argXML, "      <string>%s</string>\n", xmlText(arg))
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>%s</string>
  <key>ProgramArguments</key>
  <array>
%s  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key>
    <string>%s</string>
    <key>PATH</key>
    <string>%s</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ProcessType</key>
  <string>Background</string>
  <key>ThrottleInterval</key>
  <integer>60</integer>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, serviceLabel, argXML.String(), xmlText(o.Home), xmlText(o.Path), xmlText(o.StdoutPath), xmlText(o.StderrPath))
}

func xmlText(s string) string {
	var b bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".limitping-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := true
	defer func() {
		_ = f.Close()
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(mode); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}
