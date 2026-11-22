package runner

import "syscall"

// sysProcAttr puts the turn in its own process group. Pdeathsig makes agentic (which forwards
// SIGTERM to claude) end the turn if the server dies without stopping it. It fires when the thread
// that started the child exits; Go only ends a thread when a goroutine exits
// while locked to it, which this program never does.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGTERM}
}
