BINARY  := denon
PKG     := ./cmd/denon
VERSION := 0.1.0
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin-arm64 darwin-amd64 linux-amd64 linux-arm64 windows-amd64

.PHONY: build build-all clean install uninstall status logs run

build:
	mkdir -p bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(PKG)

build-all: clean
	mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%-*}; arch=$${p#*-}; \
		out=dist/$(BINARY)-$$os-$$arch; \
		[ "$$os" = "windows" ] && out=$$out.exe; \
		echo "  build $$out"; \
		GOOS=$$os GOARCH=$$arch go build -ldflags "$(LDFLAGS)" -o $$out $(PKG) || exit 1; \
	done

clean:
	rm -rf bin dist

# Foreground run with non-privileged ports for development.
run: build
	./bin/$(BINARY) -http :8080 -v

# Build from this checkout and install as a LaunchDaemon (macOS) or systemd
# unit (Linux). End users should use the curl|sh one-liner from the README;
# this target is for developers iterating from source.
install: build
	./install.sh --local $(if $(BASE_URL),--base-url=$(BASE_URL),)

uninstall:
	./install.sh --uninstall

status:
	@sudo launchctl print system/com.denon.tuner 2>/dev/null | grep -E '(state|pid|last exit)' || echo "not installed"

logs:
	tail -f /var/log/denon.out.log /var/log/denon.err.log
