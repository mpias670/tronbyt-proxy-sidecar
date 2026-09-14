//go:build linux

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"
)

// reapZombies runs a background loop that waits on any orphaned child
// process. When the sidecar runs as PID 1 in a container (the common case
// for a minimal single-process image with no supervisor like tini/dumb-init),
// the kernel reparents a headless Chromium's grandchild processes to PID 1
// once chromedp kills the direct child, and PID 1 is responsible for
// reaping them via wait() or they pile up as zombies. Zombies don't leak
// real memory, but they're needless process-table clutter and worth
// avoiding cleanly rather than relying on every deployment to remember
// `docker run --init` / `init: true`.
func reapZombies() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGCHLD)
	go func() {
		for range sigs {
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				log.Printf("reaped orphaned child process pid=%d", pid)
			}
		}
	}()
}
