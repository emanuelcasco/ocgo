//go:build windows

package main

import "syscall"

func detachedAttrs() *syscall.SysProcAttr { return nil }
