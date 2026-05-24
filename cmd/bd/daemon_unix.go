//go:build unix

package main

import "syscall"

// daemonProcAttrDetached starts the daemon child in its own session so it
// survives the parent's exit (no controlling terminal, not in the parent's
// process group).
func daemonProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
