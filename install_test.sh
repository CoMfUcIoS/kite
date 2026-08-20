#!/bin/sh
# Self-test for install.sh's helpers. No network, no installing.
set -eu

# shellcheck source=install.sh
KITE_INSTALL_LIB=1 . "$(dirname "$0")/install.sh"

failures=0

expect_gt() {
	if version_gt "$1" "$2"; then
		echo "ok   $1 > $2"
	else
		echo "FAIL $1 > $2 should hold"
		failures=$((failures + 1))
	fi
}

expect_not_gt() {
	if version_gt "$1" "$2"; then
		echo "FAIL $1 > $2 should not hold"
		failures=$((failures + 1))
	else
		echo "ok   $1 !> $2"
	fi
}

expect_gt v0.2.0 v0.1.0
expect_gt v1.0.0 v0.9.9
expect_gt v0.1.10 v0.1.9      # not a string comparison
expect_gt 0.2.0 v0.1.0        # a missing v prefix is fine either side
expect_gt v0.1.0 devel+abc123 # any release beats a local build

expect_not_gt v0.1.0 v0.1.0
expect_not_gt v0.1.0 v0.2.0
expect_not_gt v0.1.9 v0.1.10
expect_not_gt v0.2 v0.2.0     # a missing component reads as zero, so these are equal
expect_not_gt v0.2.0 v0.2     # ...in both directions
expect_not_gt devel+abc123 v0.1.0

# platform must resolve to something the release actually publishes.
target=$(platform)
case "$target" in
linux_amd64 | linux_arm64 | darwin_amd64 | darwin_arm64)
	echo "ok   platform = $target"
	;;
*)
	echo "FAIL platform = $target is not a published asset name"
	failures=$((failures + 1))
	;;
esac

if [ "$failures" -ne 0 ]; then
	echo "$failures failure(s)"
	exit 1
fi
echo "install.sh self-test passed"
