.PHONY: check test vet build demo deps

check: vet test deps

vet:
	go vet ./...

test:
	go test ./... -count=1 -timeout 120s

build:
	CGO_ENABLED=0 go build ./...

# 依赖面门禁：纯判据包不得链数据库驱动（issue #10）——
# 驱动边只许待在绑定包（如 storage/sqlitestore），不许进 storage 这类纯判据包。
deps:
	bash scripts/check-dep-edges.sh

demo:
	CGO_ENABLED=0 go run ./examples/embedded-demo
	CGO_ENABLED=0 go run ./examples/standalone-demo
