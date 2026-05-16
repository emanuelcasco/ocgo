package main

import (
	"errors"
	"strconv"
	"strings"
)

func parseWindowsNetstatPID(output string, port int) (int, error) {
	wanted := strconv.Itoa(port)
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.EqualFold(fields[0], "tcp") {
			continue
		}
		if !strings.EqualFold(fields[len(fields)-2], "listening") {
			continue
		}
		if !netstatAddressUsesPort(fields[1], wanted) {
			continue
		}
		pid, err := strconv.Atoi(fields[len(fields)-1])
		if err == nil && pid > 0 {
			return pid, nil
		}
	}
	return 0, errors.New("no listener found")
}

func netstatAddressUsesPort(address, port string) bool {
	address = strings.TrimSpace(address)
	if address == "" {
		return false
	}
	if strings.HasPrefix(address, "[") {
		return strings.HasSuffix(address, "]:"+port)
	}
	idx := strings.LastIndex(address, ":")
	return idx >= 0 && address[idx+1:] == port
}
