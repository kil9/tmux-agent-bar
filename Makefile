BINARY := tmux-agent-bar
INSTALL_DIR := $(HOME)/.local/bin

build:
	go build -o $(BINARY) .

install: build
	install -m 755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

clean:
	rm -f $(BINARY)

.PHONY: build install clean
