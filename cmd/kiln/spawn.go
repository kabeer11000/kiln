package main

import (
	"os"
	"os/exec"

	"github.com/kabeer11000/kiln/internal/kiln"
)

// spawnSupervisor starts `kiln _supervise <id>` as a background process that outlives this one.
func spawnSupervisor(s kiln.Store, id string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	out, err := os.Create(s.OutPath(id))
	if err != nil {
		return err
	}
	defer out.Close() // the child keeps its own handle
	cmd := exec.Command(exe, "_supervise", id)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
