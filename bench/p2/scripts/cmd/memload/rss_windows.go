//go:build windows

package main

// Windows 侧直接用 kernel32/psapi 读 RSS。
//
// 为什么不用 tasklist 子进程：每次采样要起一个进程（本机实测 300ms+），
// 采样间隔 200ms 时会把曲线抹平成「一两个点」，20 并发的峰值可能整个漏掉。
// 直接调 API 是微秒级，且不引入新依赖（syscall 是标准库）。
//
// 为什么不用 golang.org/x/sys/windows：它在 go.mod 里是 indirect，
// import 后需把 go.mod 改成 direct，而本任务的改动范围不含 go.mod。

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	psapi                 = syscall.NewLazyDLL("psapi.dll")
	procOpenProcess       = kernel32.NewProc("OpenProcess")
	procCloseHandle       = kernel32.NewProc("CloseHandle")
	procGetProcessMemInfo = psapi.NewProc("GetProcessMemoryInfo")
)

const (
	processQueryLimitedInformation = 0x1000
	processVMRead                  = 0x0010
)

// processMemoryCounters 对应 PROCESS_MEMORY_COUNTERS（psapi.h）。
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// readRSS 返回进程 RSS（MB）。
func readRSS(pid int) (float64, error) {
	h, _, err := procOpenProcess.Call(
		uintptr(processQueryLimitedInformation|processVMRead),
		0,
		uintptr(pid),
	)
	if h == 0 {
		return 0, fmt.Errorf("OpenProcess(%d): %v", pid, err)
	}
	defer procCloseHandle.Call(h)

	var pmc processMemoryCounters
	pmc.CB = uint32(unsafe.Sizeof(pmc))
	ok, _, err := procGetProcessMemInfo.Call(h, uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.CB))
	if ok == 0 {
		return 0, fmt.Errorf("GetProcessMemoryInfo(%d): %v", pid, err)
	}
	return float64(pmc.WorkingSetSize) / (1024 * 1024), nil
}
