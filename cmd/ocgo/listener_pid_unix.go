//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
)

func findListenerPID(port int) (int, error) {
	if port == 0 {
		return 0, errors.New("missing port")
	}
	out, err := exec.Command("lsof", "-nP", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pid, err := strconv.Atoi(strings.TrimSpace(line))
		if err == nil && pid > 0 {
			return pid, nil
		}
	}
	return 0, errors.New("no listener found")
}
