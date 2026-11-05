.DEFAULT_GOAL := build
.NOTPARALLEL:

.PHONY: help check helper licenses build app verify archive dmg clean

help:
	@printf '%s\n' \
	  'make check    Run go vet and go test without installing the helper' \
	  'make helper   Build the ARM64 helper for development and packaging' \
	  'make licenses Collect reviewed licenses for the two ARM64 binaries' \
	  'make build    Build and verify the ad hoc signed ARM64 app (no DMG)' \
	  'make verify   Verify an existing app bundle and its helper' \
	  'make archive  Build the app and a ZIP preserving executable permissions' \
	  'make dmg      Build the app and DMG (temporarily mounts a disk image)' \
	  'make clean    Remove generated bundles, archives and the built helper'

check helper licenses verify archive dmg clean:
	@/bin/bash scripts/build.sh $@

build app:
	@/bin/bash scripts/build.sh app
