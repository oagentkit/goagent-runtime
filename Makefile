.PHONY: check test vet build demo

check: vet test

vet:
	go vet ./...

test:
	go test ./... -count=1 -timeout 120s

build:
	CGO_ENABLED=0 go build ./...

demo:
	CGO_ENABLED=0 go run ./examples/embedded-demo
	CGO_ENABLED=0 go run ./examples/standalone-demo
