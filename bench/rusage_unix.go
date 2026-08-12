//go:build darwin || linux

package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

// maxRSSKB reads the finished command's peak resident set size in KiB.
// ru_maxrss is bytes on darwin and KiB on linux.
func maxRSSKB(cmd *exec.Cmd) int64 {
	if cmd.ProcessState == nil {
		return 0
	}
	ru, ok := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	if !ok || ru == nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return ru.Maxrss / 1024
	}
	return ru.Maxrss
}
