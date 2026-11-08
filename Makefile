# kuma-sla — build & deploy
BINARY  := kuma-sla-linux-amd64
LDFLAGS := -s -w
IMAGE   := registry.gutocarvalho.net/images/kuma-sla:latest

# Deploy target (override on the command line): make deploy HOST=myhost
HOST      ?= kuma-claude
REMOTE_DIR ?= /opt/kuma-sla

.PHONY: build vet test run image deploy clean

build: ## build a static linux/amd64 binary
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

vet: ## go vet + gofmt check (fails on unformatted files, like CI)
	go vet ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

test: ## go test
	go test ./...

run: ## run locally (KUMA_DB=... CONFIG_DIR=...)
	go run .

image: ## build the container image locally
	docker build -t $(IMAGE) .

deploy: ## pull the latest image and (re)start the container on HOST via compose
	ssh $(HOST) 'cd $(REMOTE_DIR) && docker compose pull && docker compose up -d && docker compose ps'

clean:
	rm -f $(BINARY)
