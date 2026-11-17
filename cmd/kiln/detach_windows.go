package main

import "syscall"

// CREATE_NO_WINDOW gives the background kiln a hidden console that every tool it runs (aws,
// docker) inherits, so nothing ever opens a window. (DETACHED_PROCESS would leave it with no
// console at all, and Windows then opens a new visible window for each console child.)
// A new process group keeps Ctrl-C in the launching terminal from reaching it.
func detachAttr() *syscall.SysProcAttr {
	const createNoWindow = 0x08000000
	return &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | createNoWindow, HideWindow: true}
}
