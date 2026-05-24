//go:build windows

package main

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// daemonProcAttrDetached starts the daemon child detached from the parent
// console. Note: daemon mode itself is unsupported on Windows (the socket
// transport returns an error); this exists so the command package compiles.
func daemonProcAttrDetached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
