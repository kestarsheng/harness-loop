package parser

import "strings"

// 语言标识。只有这里登记的语言才会被索引器识别为可结构化解析。
const (
	LangGo         = "go"
	LangTypeScript = "typescript"
	LangPython     = "python"
	LangUnknown    = "unknown"
)

// Capability 表示当前构建对某语言的解析能力级别。
// full  = 语法树（Go AST / tree-sitter）解析；
// regex = 正则启发式解析（无 cgo 或未启用 treesitter 标签时的降级路径）；
// none  = 不支持，只能降级为文件级 chunk。
type Capability string

const (
	CapabilityFull  Capability = "full"
	CapabilityRegex Capability = "regex"
	CapabilityNone  Capability = ""
)

// Language 是语言注册表条目：名字 + 扩展名。
type Language struct {
	Name       string
	Extensions []string
}

// Languages 是索引器支持的全部语言。新增语言时同步 registry 里的 parser 注册。
var Languages = []Language{
	{Name: LangGo, Extensions: []string{".go"}},
	{Name: LangTypeScript, Extensions: []string{".ts", ".tsx"}},
	{Name: LangPython, Extensions: []string{".py"}},
}

var regexParsedLanguages = map[string]bool{
	LangTypeScript: true,
	LangPython:     true,
}

func supportsRegex(lang string) bool { return regexParsedLanguages[lang] }

func supportsTreeSitter(lang string) bool { return regexParsedLanguages[lang] }

// CapabilityFor 返回当前构建对 lang 的能力级别。
func CapabilityFor(lang string) Capability {
	if lang == LangGo {
		return CapabilityFull
	}
	if treesitterAvailable && supportsTreeSitter(lang) {
		return CapabilityFull
	}
	if supportsRegex(lang) {
		return CapabilityRegex
	}
	return CapabilityNone
}

// Capabilities 导出当前构建的全部语言能力，供索引状态/运维接口展示。
func Capabilities() map[string]Capability {
	out := make(map[string]Capability, len(Languages))
	for _, language := range Languages {
		out[language.Name] = CapabilityFor(language.Name)
	}
	return out
}

// TreesitterEnabled 报告本次构建是否编入了 tree-sitter 语法解析。
func TreesitterEnabled() bool { return treesitterAvailable }

// LanguageOfPath 按扩展名反查注册表语言名。
func LanguageOfPath(path string) string {
	lower := strings.ToLower(path)
	for _, language := range Languages {
		for _, ext := range language.Extensions {
			if strings.HasSuffix(lower, ext) {
				return language.Name
			}
		}
	}
	return LangUnknown
}
