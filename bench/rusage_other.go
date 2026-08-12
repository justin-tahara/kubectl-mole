//go:build !(darwin || linux)

package main

import "os/exec"

// The bench runs on darwin and linux; elsewhere RSS reads as absent.
func maxRSSKB(*exec.Cmd) int64 { return 0 }
