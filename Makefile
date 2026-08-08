.PHONY: build test lint dist clean

VERSION ?= $(shell git describe --tags --always 2>/dev/null || echo dev)
LDFLAGS := -X github.com/lykuroai/Native-LLM-Platform/gwcore.Version=$(VERSION)
DIST    := dist/$(VERSION)

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/private-gateway ./cmd/private-gateway/

test:
	go test ./...

lint:
	gofmt -l . && go vet ./...

# Windows / Linux / macOS 向け配布バイナリ(amd64/arm64)+ checksums.txt
dist:
	@mkdir -p $(DIST)
	GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/private-gateway_$(VERSION)_linux_amd64 ./cmd/private-gateway/
	GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/private-gateway_$(VERSION)_linux_arm64 ./cmd/private-gateway/
	GOOS=darwin  GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/private-gateway_$(VERSION)_darwin_amd64 ./cmd/private-gateway/
	GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/private-gateway_$(VERSION)_darwin_arm64 ./cmd/private-gateway/
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags '$(LDFLAGS)' -o $(DIST)/private-gateway_$(VERSION)_windows_amd64.exe ./cmd/private-gateway/
	# Windows は zip で配布(ブラウザの無署名exe直DLブロック回避。-m で元exeを削除)
	cd $(DIST) && zip -q -m private-gateway_$(VERSION)_windows_amd64.zip private-gateway_$(VERSION)_windows_amd64.exe
	cd $(DIST) && shasum -a 256 private-gateway_* > checksums.txt

clean:
	rm -rf bin dist
