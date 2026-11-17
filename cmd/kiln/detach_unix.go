//go:build !windows

package main

import "syscall"

// A new session: no controlling terminal, so closing the terminal doesn't hang it up.
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
