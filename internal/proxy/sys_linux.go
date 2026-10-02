package proxy

import "syscall"

// Pdeathsig makes the kernel signal the server if the proxy dies without running its
// cleanup (SIGKILL from the client). It fires when the *thread* that forked exits, which
// is why Proxy.Run locks its goroutine to an OS thread for its whole lifetime.
//
// It reaches the direct child only, not the rest of its process group. Helpers that
// outlive it rely on their own parent-death handling — which is what they do without
// the proxy too. For chrome-devtools-mcp (watchdog and browser in groups of their own)
// all 16 descendants were gone within 8 s of a kill -9 of the proxy.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
