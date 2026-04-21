.DEFAULT_GOAL := cli

.PHONY: cli tests

APP_NAME := spexus-agent
BIN_DIR := bin
BIN_PATH := $(BIN_DIR)/$(APP_NAME)

cli: tests
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_PATH) ./cmd/$(APP_NAME)

tests:
	go test ./...
