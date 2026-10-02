//go:build unix

package proxy

import "syscall"

func killGroup(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig)
}
