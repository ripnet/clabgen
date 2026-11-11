package main

import (
	"os"
	"os/signal"
	"syscall"
)

// reapChildren: as a container's PID 1, the agent inherits the orphans of
// commands run in its container (docker exec … timeout …), and nothing else
// waits for them: they'd stay zombies. The agent starts no processes of its
// own, so it can wait for any child.
func reapChildren() {
	if os.Getpid() != 1 {
		return
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for {
			for {
				var ws syscall.WaitStatus
				if pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil); pid <= 0 || err != nil {
					break
				}
			}
			<-ch
		}
	}()
}
