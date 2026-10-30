//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

func rssBytes() int64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			f := strings.Fields(strings.TrimPrefix(l, "VmRSS:"))
			if len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				return kb << 10
			}
		}
	}
	return 0
}
