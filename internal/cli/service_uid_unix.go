//go:build !windows

package cli

import "os"

func currentUID() int { return os.Getuid() }
