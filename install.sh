#!/bin/sh
# Install or update kite.
#
#   curl -fsSL https://raw.githubusercontent.com/comfucios/kite/main/install.sh | sh
#
# Installs the latest release for this OS and architecture, updates an older
# copy in place, and does nothing at all if the newest release is already there.
#
# Environment:
#   KITE_VERSION      install this exact tag instead of the latest (e.g. v0.2.0)
#   KITE_INSTALL_DIR  where to put the binary (default: ~/.local/bin)

set -eu

REPO="comfucios/kite"
BIN="kite"
INSTALL_DIR="${KITE_INSTALL_DIR:-$HOME/.local/bin}"

die() {
	printf 'kite install: %s\n' "$*" >&2
	exit 1
}

# platform prints the os_arch pair matching the release asset names.
platform() {
	os=$(uname -s | tr '[:upper:]' '[:lower:]')
	case "$os" in
	linux | darwin) ;;
	*) die "unsupported operating system: $os. Build from source with: go install github.com/comfucios/kite@latest" ;;
	esac

	arch=$(uname -m)
	case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture: $arch. Build from source with: go install github.com/comfucios/kite@latest" ;;
	esac

	printf '%s_%s' "$os" "$arch"
}

# latest_tag reads the tag from the URL that /releases/latest redirects to.
# That avoids the API, so there is no rate limit and no token to supply.
latest_tag() {
	url=$(curl -fsSL -o /dev/null -w '%{url_effective}' \
		"https://github.com/$REPO/releases/latest") || return 1
	tag=${url##*/}
	case "$tag" in
	"" | latest | *releases*) return 1 ;;
	esac
	printf '%s' "$tag"
}

# version_gt succeeds when $1 is strictly newer than $2, comparing the dotted
# numbers only. A local build reporting "devel+abc1234" reads as 0, so any
# real release counts as newer than it.
version_gt() {
	awk -v a="${1#v}" -v b="${2#v}" 'BEGIN {
		na = split(a, x, ".")
		nb = split(b, y, ".")
		n = na > nb ? na : nb
		for (i = 1; i <= n; i++) {
			if ((x[i] + 0) > (y[i] + 0)) exit 0
			if ((x[i] + 0) < (y[i] + 0)) exit 1
		}
		exit 1
	}'
}

installed_version() {
	command -v "$BIN" >/dev/null 2>&1 || return 1
	"$BIN" --version 2>/dev/null | awk 'NR == 1 {print $2}'
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

main() {
	command -v curl >/dev/null 2>&1 || die "curl is required"
	command -v awk >/dev/null 2>&1 || die "awk is required"

	target=$(platform)

	tag="${KITE_VERSION:-}"
	if [ -z "$tag" ]; then
		tag=$(latest_tag) || die "could not find the latest release of $REPO.
              If the repository is private, its releases are not reachable
              without a token. Install from source instead:
                go install github.com/comfucios/kite@latest"
	fi

	current=$(installed_version || true)
	if [ -n "$current" ]; then
		if [ "$current" = "$tag" ]; then
			echo "kite $current is already the latest release. Nothing to do."
			return 0
		fi
		if version_gt "$current" "$tag"; then
			echo "kite $current is newer than the latest release ($tag). Nothing to do."
			return 0
		fi
		echo "Updating kite $current -> $tag"
	else
		echo "Installing kite $tag"
	fi

	asset="${BIN}_${target}"
	base="https://github.com/$REPO/releases/download/$tag"

	tmp=$(mktemp -d) || die "could not create a temporary directory"
	trap 'rm -rf "$tmp"' EXIT HUP INT TERM

	curl -fsSL -o "$tmp/$asset" "$base/$asset" ||
		die "release $tag has no build for $target
              looked for $base/$asset"

	# Verify when both the published checksums and a hashing tool are available.
	if curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" 2>/dev/null; then
		expected=$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")
		actual=$(sha256_of "$tmp/$asset")
		if [ -n "$expected" ] && [ -n "$actual" ] && [ "$expected" != "$actual" ]; then
			die "checksum mismatch for $asset
              expected $expected
              got      $actual"
		fi
	fi

	mkdir -p "$INSTALL_DIR" || die "could not create $INSTALL_DIR"
	chmod +x "$tmp/$asset"
	mv -f "$tmp/$asset" "$INSTALL_DIR/$BIN" ||
		die "could not write to $INSTALL_DIR. Set KITE_INSTALL_DIR to a writable directory."

	echo "Installed $("$INSTALL_DIR/$BIN" --version) to $INSTALL_DIR/$BIN"

	case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		echo
		echo "$INSTALL_DIR is not on your PATH. Add it with:"
		echo "  export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
	esac
}

# Sourcing this file with KITE_INSTALL_LIB=1 defines the helpers without
# installing anything, which is how install_test.sh exercises them.
if [ "${KITE_INSTALL_LIB:-}" != "1" ]; then
	main
fi
