BIN     := runnermaxxer
LDFLAGS := -s -w

.PHONY: build run test test-integration vet golden snapshot clean

build:
	go build -trimpath -ldflags '$(LDFLAGS)' -o bin/$(BIN) ./cmd/$(BIN)

run: build
	./bin/$(BIN) --dir .

test:
	go test ./...

test-integration:
	go test -tags integration ./...

vet:
	go vet ./...

golden:
	go test ./internal/ui -run Golden -update

snapshot:
	goreleaser release --snapshot --clean

clean:
	rm -rf bin dist
