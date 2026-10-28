//go:build unix

package main

import (
	"os"
	"syscall"
)

func forwardedSignals() []os.Signal {
	return []os.Signal{
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGHUP,
		syscall.SIGQUIT,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
	}
}
