//go:build !windows && !linux

package player

func daemons(string) []int { return nil }
