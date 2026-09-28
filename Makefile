.PHONY: test build

test:
	./tests/run.sh
	$(MAKE) -C tui test

build:
	$(MAKE) -C tui build
