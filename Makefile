# kuma-sla — build & deploy
BINARY  := kuma-sla-linux-amd64
LDFLAGS := -s -w

# Deploy target (override on the command line):
#   make deploy HOST=myhost
HOST        ?= kuma-claude
REMOTE_BIN  ?= /opt/kuma-sla/kuma-sla
REMOTE_CFG  ?= /opt/kuma-sla/domains

.PHONY: build vet test run deploy clean

build: ## build a static linux/amd64 binary
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

vet: ## go vet
	go vet ./...

test: ## go test
	go test ./...

run: ## run locally against a DB (KUMA_DB=... CONFIG_DIR=...)
	go run .

deploy: build ## build, copy binary + local domain configs to HOST, restart service
	scp $(BINARY) $(HOST):/tmp/kuma-sla
	ssh $(HOST) 'sudo install -m0755 /tmp/kuma-sla $(REMOTE_BIN) && sudo install -d $(REMOTE_CFG)'
	@if ls domains/*.yaml >/dev/null 2>&1; then \
	  scp domains/*.yaml $(HOST):/tmp/ && \
	  ssh $(HOST) 'sudo install -m0644 /tmp/*.yaml $(REMOTE_CFG)/'; \
	fi
	ssh $(HOST) 'sudo systemctl restart kuma-sla && systemctl is-active kuma-sla'

clean:
	rm -f $(BINARY)
