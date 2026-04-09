BINARY := tmux-agent-bar

build:
	go build -o $(BINARY) .

clean:
	rm -f $(BINARY)

.PHONY: build clean
