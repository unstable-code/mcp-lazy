//go:build unix && !linux

package proxy

import "syscall"

// No Pdeathsig outside Linux: if the proxy is SIGKILLed the server is left to notice
// its closed stdin, which is what an unwrapped server would do anyway.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
