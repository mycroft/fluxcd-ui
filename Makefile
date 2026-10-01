VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE ?= registry.mkz.me/mycroft/fluxcd-ui
TAG ?= dev
# The Dockerfile needs BuildKit: docker with buildx, or podman.
CONTAINER_TOOL ?= docker

TAILWIND_VERSION ?= v4.3.3
HTMX_VERSION ?= 2.0.11
HTMX_SSE_VERSION ?= 2.2.4

BIN := $(CURDIR)/bin
TAILWIND := $(BIN)/tailwindcss-$(TAILWIND_VERSION)
CSS_IN := internal/web/styles/app.css
CSS_OUT := internal/web/static/css/app.css
JS_DIR := internal/web/static/js

TAILWIND_OS := $(if $(filter Darwin,$(shell uname -s)),macos,linux)
TAILWIND_ARCH := $(if $(filter arm64 aarch64,$(shell uname -m)),arm64,x64)

.PHONY: build tailwind css css-watch run test vet lint docker-build helm-lint vendor-js clean

build: css
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $(BIN)/fluxcd-ui ./cmd/fluxcd-ui

$(TAILWIND):
	mkdir -p $(BIN)
	curl -fsSL -o $@ https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TAILWIND_OS)-$(TAILWIND_ARCH)
	chmod +x $@

tailwind: $(TAILWIND)

css: $(TAILWIND)
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --minify

css-watch: $(TAILWIND)
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --watch

# Runs against the current kubeconfig context. Pass flags with ARGS="...".
run: css
	go run ./cmd/fluxcd-ui $(ARGS)

test:
	go test ./...

vet:
	go vet ./...

lint:
	golangci-lint run -c .golangci.yaml ./...

docker-build:
	$(CONTAINER_TOOL) build --build-arg VERSION=$(VERSION) --build-arg TAILWIND_VERSION=$(TAILWIND_VERSION) -t $(IMAGE):$(TAG) .

helm-lint:
	helm lint charts/fluxcd-ui

# Vendored htmx files are committed; this refreshes them to the pinned versions.
vendor-js:
	curl -fsSL -o $(JS_DIR)/htmx.min.js https://cdn.jsdelivr.net/npm/htmx.org@$(HTMX_VERSION)/dist/htmx.min.js
	curl -fsSL -o $(JS_DIR)/sse.js https://cdn.jsdelivr.net/npm/htmx-ext-sse@$(HTMX_SSE_VERSION)/dist/sse.min.js

clean:
	rm -rf $(BIN) $(CSS_OUT)
