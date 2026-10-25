//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd

package net

func processAlive(int) bool { return true }
