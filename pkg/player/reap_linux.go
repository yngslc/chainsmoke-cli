package player

import (
	"os"
	"slices"
	"strconv"
	"strings"
)

func daemons(arg string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil || strings.TrimSpace(string(comm)) != "mpv" {
			continue
		}
		argv, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err == nil && slices.Contains(strings.Split(string(argv), "\x00"), arg) {
			pids = append(pids, pid)
		}
	}
	return pids
}
