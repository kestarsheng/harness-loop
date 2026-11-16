// Package main 是验收压测的固定工作区。
//
// 它必须小到 go build 在 1s 内跑完，否则 DFX-P-06 的 10s 预算会被
// 无关的编译时间吃掉，测到的就不是验收链路本身的开销。
// 只用标准库，保证离线可构建。
package main

import "fmt"

func main() {
	fmt.Println(Verify(41))
}

// Verify 是 build_pass 判据落地的目标符号，保持稳定。
func Verify(n int) int {
	if n < 0 {
		return 0
	}
	return n + 1
}
