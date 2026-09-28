package player

import (
	"slices"
	"strings"
	"syscall"
	"unsafe"
)

// The process table, read through kernel32 and ntdll rather than
// golang.org/x/sys, for the same reason pkg/core/term_windows.go gives: one
// dependency is a property worth keeping.
var (
	ntdll                         = syscall.NewLazyDLL("ntdll.dll")
	procNtQueryInformationProcess = ntdll.NewProc("NtQueryInformationProcess")
)

const (
	processCommandLineInformation  = 60         // PROCESSINFOCLASS, Windows 8.1+
	processQueryLimitedInformation = 0x1000     // enough to read a command line
	statusInfoLengthMismatch       = 0xC0000004 // NTSTATUS: buffer too small
)

// daemons lists the mpv processes whose command line carries arg. Both halves
// of a daemon match: PATHEXT puts .COM first, so `mpv` starts the mpv.com
// console wrapper, which hands its own command line to the mpv.exe it spawns.
//
// Splitting on whitespace is exact for our endpoint: a pipe name with no
// spaces is never quoted on a command line.
func daemons(arg string) []int {
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)

	var pids []int
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		switch strings.ToLower(syscall.UTF16ToString(e.ExeFile[:])) {
		case "mpv.exe", "mpv.com":
			if slices.Contains(strings.Fields(commandLine(e.ProcessID)), arg) {
				pids = append(pids, int(e.ProcessID))
			}
		}
	}
	return pids
}

// commandLine reads another process's command line, or returns "" when it
// cannot: a process that already exited, or one another user owns. Neither is
// ours to kill.
func commandLine(pid uint32) string {
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)

	buf := make([]byte, 1024)
	for {
		var need uint32
		st, _, _ := procNtQueryInformationProcess.Call(uintptr(h), processCommandLineInformation,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&need)))
		if uint32(st) == 0 {
			break
		}
		if uint32(st) != statusInfoLengthMismatch || int(need) <= len(buf) {
			return ""
		}
		buf = make([]byte, need)
	}
	// The reply is a UNICODE_STRING whose Buffer points further into buf.
	us := (*struct {
		Length, MaximumLength uint16
		Buffer                *uint16
	})(unsafe.Pointer(&buf[0]))
	return syscall.UTF16ToString(unsafe.Slice(us.Buffer, us.Length/2))
}
