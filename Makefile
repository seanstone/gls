BINARY := gls
PREFIX := $(HOME)/.local
BINDIR := $(PREFIX)/bin

.PHONY: build install clean

build:
	go build -o $(BINARY) .

install: build
	mkdir -p $(BINDIR)
	install -m 0755 $(BINARY) $(BINDIR)/$(BINARY)

clean:
	rm -f $(BINARY)
