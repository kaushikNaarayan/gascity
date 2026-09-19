//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

const providerOpProgressSupported = true

// sampleProcessGroupProgress sums rchar+wchar (bytes) and syscr+syscw over every process
// whose process group is pgid. Socket reads/writes count, so a bd child that is
// stepping through migration statements against Dolt advances this counter even
// though its own disk I/O is nil; a hung child does not.
func sampleProcessGroupProgress(pgid int) (providerOpProgressSample, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return providerOpProgressSample{}, false
	}
	var total providerOpProgressSample
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(stat)
		i := strings.LastIndexByte(s, ')')
		if i < 0 {
			continue
		}
		// after ")": state ppid pgrp ...
		f := strings.Fields(s[i+1:])
		if len(f) < 3 || f[2] != strconv.Itoa(pgid) {
			continue
		}
		io, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/io")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(io), "\n") {
			k, v, ok := strings.Cut(line, ": ")
			if !ok {
				continue
			}
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			if err != nil {
				continue
			}
			switch k {
			case "rchar", "wchar":
				total.Bytes += n
			case "syscr", "syscw":
				total.Syscalls += n
			}
		}
	}
	return total, true
}
