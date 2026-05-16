//go:build windows

package main

import (
	"errors"
	"os/exec"
)

func findListenerPID(port int) (int, error) {
	if port == 0 {
		return 0, errors.New("missing port")
	}
	out, err := exec.Command("netstat", "-ano", "-p", "tcp").Output()
	if err != nil {
		return 0, err
	}
	return parseWindowsNetstatPID(string(out), port)
}
