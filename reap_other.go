//go:build !linux

package main

// reapZombies is a no-op on non-Linux platforms: this sidecar only needs to
// self-reap when running as an unsupervised PID 1 inside a Linux container.
func reapZombies() {}
