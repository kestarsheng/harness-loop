//go:build !(cgo && treesitter)

package parser

// treesitterAvailable 在未启用 `-tags=treesitter` 或 CGO 关闭时为 false，
// 多语言解析走 regex.go 的正则降级路径。此文件必须保证无 cgo 环境可编译。
const treesitterAvailable = false

// newTreeSitterParser 在降级构建中恒返回 nil，Registry 自动跳过。
func newTreeSitterParser(lang string) Parser { return nil }
