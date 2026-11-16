//go:build !windows

package main

// 非 Windows 侧从 /proc 读 RSS，语义与 Windows 分支保持一致（都是物理常驻集）。

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func readRSS(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseFloat(fields[1], 64)
				if err != nil {
					return 0, err
				}
				return kb / 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("VmRSS not found for pid %d", pid)
}
