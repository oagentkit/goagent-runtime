module github.com/yi-nology/goagent-runtime/bridge

// 嵌套模块：LLM SDK 边（llm → cloudwego/eino）只出现在 bridge 内，
// 主 module 的「模型接缝只有 chat.Model、零 LLM SDK import」声明面不变
// （与 sqliteadapter/pgstore 的驱动边分包是同一条纪律）。
//
// 发版顺序：runtime v0.7.0 先发，本模块随后以 bridge/v0.1.0 发
// （嵌套模块 tag 带子目录前缀）。
go 1.27.4

require (
	github.com/cloudwego/eino v0.9.23
	github.com/eino-contrib/jsonschema v1.0.3
	github.com/yi-nology/goagent-runtime v0.7.0
	github.com/oagentkit/llm v0.27.0
)

require (
	github.com/bahlo/generic-list-go v0.2.0 // indirect
	github.com/buger/jsonparser v1.6.1 // indirect
	github.com/bytedance/gopkg v0.1.4 // indirect
	github.com/bytedance/sonic v1.15.5 // indirect
	github.com/bytedance/sonic/loader v0.5.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.7 // indirect
	github.com/cloudwego/eino-ext/components/model/openai v0.1.13 // indirect
	github.com/cloudwego/eino-ext/libs/acl/openai v0.1.17 // indirect
	github.com/dustin/go-humanize v1.1.0 // indirect
	github.com/evanphx/json-patch v0.5.2 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/goph/emperror v0.17.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.4.2 // indirect
	github.com/mailru/easyjson v0.9.2 // indirect
	github.com/meguminnnnnnnnn/go-openai v0.1.6 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/nikolalohinski/gonja v1.5.3 // indirect
	github.com/yi-nology/goagent-kernel v0.77.0 // indirect
	github.com/oagentkit/textx v0.12.0 // indirect
	github.com/oekit/concurrency v0.40.0 // indirect
	github.com/oekit/logx v0.12.1 // indirect
	github.com/oekit/pkg v0.61.4 // indirect
	github.com/pelletier/go-toml/v2 v2.4.3 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/sirupsen/logrus v1.10.2 // indirect
	github.com/slongfield/pyfmt v0.0.0-20220222012616-ea85ff4c361f // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/wk8/go-ordered-map/v2 v2.1.8 // indirect
	github.com/yargevad/filepathx v1.0.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/metric v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	golang.org/x/arch v0.33.0 // indirect
	golang.org/x/exp v0.0.0-20261109185158-a52e3aef5920 // indirect
	golang.org/x/sys v0.50.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
