.PHONY: start test build

start:
	./start

test:
	./tests/run.sh
	$(MAKE) -C tui test

build:
	$(MAKE) -C tui build
