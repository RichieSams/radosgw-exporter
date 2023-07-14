all: image

ifeq ($(OS),Windows_NT)
export SHELL=cmd
DETECTED_OS=windows
else
DETECTED_OS=$(shell uname -s | tr '[:upper:]' '[:lower:]')
endif

export IMAGENAME=ghcr.io/richiesams/radosgw-exporter
TAG:=$(shell git describe --tags --dirty 2>/dev/null || echo v0.0.0)


###########
# Creating a docker image
###########

image:
ifeq ($(DETECTED_OS),windows)
	cmd /C "set GORELEASER_CURRENT_TAG=$(TAG) && goreleaser release --snapshot --clean"
else
	GORELEASER_CURRENT_TAG=$(TAG) goreleaser release --snapshot --clean
endif


###########
# Local testing with docker-compose
###########

run:
	docker run --rm -it \
		--env-file .env \
		-p 8080:8080 \
		$(IMAGENAME):$(TAG)


###########
# Update dependencies
###########

vendor:
	go mod tidy
