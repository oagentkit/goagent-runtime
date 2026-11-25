module github.com/oagentkit/goagent-runtime/bridge

// 嵌套模块：LLM SDK 边（llm → cloudwego/eino）只出现在 bridge 内，
// 主 module 的「模型接缝只有 chat.Model、零 LLM SDK import」声明面不变
// （与 sqliteadapter/pgstore 的驱动边分包是同一条纪律）。
//
// 发版顺序：runtime v0.7.0 先发，本模块随后以 bridge/v0.1.0 发
// （嵌套模块 tag 带子目录前缀）。
go 1.27.4

require (
	github.com/cloudwego/eino v0.9.21
	github.com/oagentkit/goagent-runtime v0.7.0
	github.com/oagentkit/llm v0.27.0
)
