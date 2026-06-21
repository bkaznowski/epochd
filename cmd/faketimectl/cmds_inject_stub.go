//go:build !linux

package main

import "fmt"

func cmdInject(_ []string) error {
	return fmt.Errorf("inject: only supported on Linux (requires ptrace)")
}

func cmdReset(_ []string) error {
	return fmt.Errorf("reset: only supported on Linux (requires ptrace)")
}
