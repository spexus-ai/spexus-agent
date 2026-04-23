.DEFAULT_GOAL := cli

.PHONY: build cli install install-service restart-service tests write-service-env

APP_NAME := spexus-agent
BIN_DIR := bin
BIN_PATH := $(BIN_DIR)/$(APP_NAME)
LOCAL_BIN_DIR ?= $(HOME)/.local/bin
INSTALL_PATH := $(LOCAL_BIN_DIR)/$(APP_NAME)
USER_SYSTEMD_DIR ?= $(HOME)/.config/systemd/user
SERVICE_NAME := $(APP_NAME).service
SERVICE_PATH := $(USER_SYSTEMD_DIR)/$(SERVICE_NAME)
SERVICE_CONFIG_DIR ?= $(HOME)/.config/$(APP_NAME)
SERVICE_ENV_PATH := $(SERVICE_CONFIG_DIR)/runtime.env

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_PATH) ./cmd/$(APP_NAME)

cli: tests build

install: build
	mkdir -p $(LOCAL_BIN_DIR)
	install -m 755 $(BIN_PATH) $(INSTALL_PATH)

write-service-env:
	mkdir -p $(SERVICE_CONFIG_DIR)
	{ \
		printf 'HOME=%s\n' "$(HOME)"; \
		printf 'PATH=%s\n' "$$PATH"; \
		printf 'SPEXUS_AGENT_ACPX_BIN=%s\n' "$$(command -v acpx)"; \
		if [ -n "$$CODEX_HOME" ]; then printf 'CODEX_HOME=%s\n' "$$CODEX_HOME"; fi; \
		if [ -n "$$XDG_CONFIG_HOME" ]; then printf 'XDG_CONFIG_HOME=%s\n' "$$XDG_CONFIG_HOME"; fi; \
		if [ -n "$$ANDROID_HOME" ]; then printf 'ANDROID_HOME=%s\n' "$$ANDROID_HOME"; fi; \
		if [ -n "$$ANDROID_SDK_ROOT" ]; then printf 'ANDROID_SDK_ROOT=%s\n' "$$ANDROID_SDK_ROOT"; fi; \
		if [ -n "$$GOPATH" ]; then printf 'GOPATH=%s\n' "$$GOPATH"; fi; \
		if [ -n "$$GOROOT" ]; then printf 'GOROOT=%s\n' "$$GOROOT"; fi; \
		if [ -n "$$NPM_CONFIG_PREFIX" ]; then printf 'NPM_CONFIG_PREFIX=%s\n' "$$NPM_CONFIG_PREFIX"; fi; \
		if [ -n "$$ANTHROPIC_API_KEY" ]; then printf 'ANTHROPIC_API_KEY=%s\n' "$$ANTHROPIC_API_KEY"; fi; \
		if [ -n "$$OPENAI_API_KEY" ]; then printf 'OPENAI_API_KEY=%s\n' "$$OPENAI_API_KEY"; fi; \
		if [ -n "$$MCP_BEARER_AUTH" ]; then printf 'MCP_BEARER_AUTH=%s\n' "$$MCP_BEARER_AUTH"; fi; \
	} > $(SERVICE_ENV_PATH)
	chmod 600 $(SERVICE_ENV_PATH)

install-service: install write-service-env
	mkdir -p $(USER_SYSTEMD_DIR)
	printf '%s\n' \
		'[Unit]' \
		'Description=Spexus Agent Runtime' \
		'After=network-online.target' \
		'Wants=network-online.target' \
		'' \
		'[Service]' \
		'Type=simple' \
		'ExecStart=$(INSTALL_PATH) runtime start' \
		'Restart=on-failure' \
		'RestartSec=5' \
		'WorkingDirectory=$(HOME)' \
		'Environment=HOME=$(HOME)' \
		'EnvironmentFile=-$(SERVICE_ENV_PATH)' \
		'' \
		'[Install]' \
		'WantedBy=default.target' \
		> $(SERVICE_PATH)
	systemctl --user daemon-reload
	systemctl --user enable --now $(SERVICE_NAME)

restart-service: write-service-env
	systemctl --user daemon-reload
	systemctl --user restart $(SERVICE_NAME)

tests:
	go test ./...
