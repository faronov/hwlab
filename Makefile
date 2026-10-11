GO ?= go
BIN := bin

.PHONY: all build proto test race vet lint fmt clean check-cgo

all: build

BUILD_PKGS := ./cmd/... ./plugins/fake/cmd/... ./plugins/ppk2/cmd/... \
		./plugins/nrf802154/cmd/... ./plugins/saleae/cmd/... ./plugins/jlink/cmd/... ./plugins/ccdebugger/cmd/... ./plugins/uart/cmd/... ./plugins/can/cmd/... ./plugins/scpi/cmd/... ./plugins/cmsisdap/cmd/... ./plugins/modbus/cmd/... ./plugins/telink/cmd/... ./plugins/picoscope/cmd/... ./plugins/usbcap/cmd/... ./plugins/ftdi/cmd/... ./plugins/espusbjtag/cmd/... ./plugins/stlink/cmd/... ./plugins/xds110/cmd/... ./plugins/jlinknative/cmd/...

build:
	$(GO) build -o $(BIN)/ $(BUILD_PKGS)
	@names=$$($(GO) list -f '{{if eq .Name "main"}}{{.ImportPath}}{{end}}' $(BUILD_PKGS) | sed 's|.*/||'); \
		if [ "$$($(GO) env GOOS)" = windows ]; then names=$$(printf '%s\n' "$$names" | sed 's/$$/.exe/'); fi; \
		sh scripts/build-ownership.sh record "$(BIN)" $$names

proto:
	protoc -I proto -I $${PROTOC_INCLUDE:-$$(dirname $$(which protoc))/../include} \
		--go_out=. --go_opt=module=github.com/faronov/hwlab \
		--go-grpc_out=. --go-grpc_opt=module=github.com/faronov/hwlab \
		proto/hwlab/plugin/v1/plugin.proto
	protoc -I third_party/saleae \
		--go_out=. --go_opt=module=github.com/faronov/hwlab \
		--go-grpc_out=. --go-grpc_opt=module=github.com/faronov/hwlab \
		third_party/saleae/saleae.proto

test:
	$(GO) test ./...

race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

# The core and every plugin except ppk2/nrf802154 must build without cgo on
# every target; those two need cgo only on darwin (see docs/adr/0026-cgo-audit.md).
check-cgo:
	CGO_ENABLED=0 $(GO) build ./...
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 $(GO) build ./...
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build ./...
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 $(GO) build ./cmd/... ./internal/app/... ./plugins/fake/... \
		./plugins/saleae/... ./plugins/jlink/... ./plugins/jlinknative/... ./plugins/ccdebugger/... ./plugins/uart/... ./plugins/can/... \
		./plugins/scpi/... ./plugins/cmsisdap/... ./plugins/ftdi/... ./plugins/espusbjtag/... ./plugins/stlink/... ./plugins/xds110/... ./plugins/modbus/... ./plugins/telink/... \
		./plugins/picoscope/... ./plugins/usbcap/...

lint:
	golangci-lint run ./...

fmt:
	gofmt -s -w .

clean:
	sh scripts/build-ownership.sh clean "$(BIN)"
