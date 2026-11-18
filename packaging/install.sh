#!/bin/sh
# Installs Snugbox from a GitHub release, after checking that the release's
# checksums were signed by its release workflow and that the archive matches
# them:
#
#   curl -fsSL https://github.com/snug-box/snugbox/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/snug-box/snugbox/releases/latest/download/install.sh | sh -s -- --version 0.4.0
#
# It prints what it will do before doing it and stops at the first mismatch.
# Downloads and checks run as you; only the copy into /usr/local runs as
# root, through sudo or doas when you are not root already. It starts
# nothing: the watcher runs only after sudo snugbox watch install.
#
# Everything runs from main on the last line, so a download cut short runs
# nothing at all.
set -eu

# The repository is the owner's account until the Snugbox GitHub organisation
# exists; .goreleaser.yaml names the same one.
REPO="snug-box/snugbox"
# cosign checks the signature. Its own SHA-256 is pinned here, read from
# cosign_checksums.txt of the release, whose signature was checked on
# 31 October 2026.
COSIGN_VERSION="v3.1.4"
COSIGN_SHA256_AMD64="424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa"
COSIGN_SHA256_ARM64="ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2"

say() { printf 'snugbox: %s\n' "$*"; }
die() {
	printf 'snugbox: %s\n' "$*" >&2
	exit 1
}

usage() {
	cat <<'EOF'
Usage: install.sh [--version X.Y.Z] [--prefix DIR] [--dry-run]

  --version X.Y.Z  install this release instead of the latest
  --prefix DIR     install under DIR instead of /usr/local
  --dry-run        print what would happen and change nothing
EOF
}

# as_root runs a command as root: directly when this already is root,
# otherwise through the tool in $elevate.
as_root() {
	if [ -z "$elevate" ]; then
		"$@"
	else
		"$elevate" "$@"
	fi
}

fetch() {
	curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$2" "$1" || die "could not download $1"
}

main() {
	prefix=/usr/local
	version=""
	dry_run=0
	while [ $# -gt 0 ]; do
		case "$1" in
		--version)
			[ $# -ge 2 ] || die "--version needs a value"
			version=$2
			shift 2
			;;
		--prefix)
			[ $# -ge 2 ] || die "--prefix needs a value"
			prefix=$2
			shift 2
			;;
		--dry-run)
			dry_run=1
			shift
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			usage >&2
			exit 2
			;;
		esac
	done

	[ "$(uname -s)" = Linux ] || die "Snugbox runs on Linux servers; this is $(uname -s)"
	case "$(uname -m)" in
	x86_64 | amd64)
		arch=amd64
		cosign_sha=$COSIGN_SHA256_AMD64
		;;
	aarch64 | arm64)
		arch=arm64
		cosign_sha=$COSIGN_SHA256_ARM64
		;;
	*) die "there is no Snugbox build for $(uname -m); releases cover x86-64 and Arm64" ;;
	esac
	case "$prefix" in
	/*) ;;
	*) die "--prefix must be an absolute path" ;;
	esac
	for tool in curl sha256sum tar install mktemp; do
		command -v "$tool" >/dev/null 2>&1 || die "this script needs $tool"
	done

	# Only the copy into the prefix needs root.
	elevate=""
	how="as root"
	if [ "$(id -u)" != 0 ]; then
		if command -v sudo >/dev/null 2>&1; then
			elevate=sudo
			how="with sudo"
		elif command -v doas >/dev/null 2>&1; then
			elevate=doas
			how="with doas"
		else
			elevate=none
		fi
	fi

	if [ -z "$version" ]; then
		version=$(curl -fsSL --proto '=https' --tlsv1.2 "https://api.github.com/repos/$REPO/releases/latest" |
			sed -n 's/^ *"tag_name": *"\([^"]*\)".*/\1/p') ||
			die "could not ask GitHub for the latest release"
		[ -n "$version" ] || die "GitHub has no published release of $REPO yet"
	fi
	version=${version#v}
	printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$' ||
		die "$version is not a release version such as 1.2.3"
	tag="v$version"
	base="https://github.com/$REPO/releases/download/$tag"
	archive="snugbox_${version}_linux_${arch}.tar.gz"
	identity="https://github.com/$REPO/.github/workflows/release.yml@refs/tags/$tag"

	say "This will:"
	say "  1. download $archive, checksums.txt and checksums.txt.sigstore.json from $base"
	say "  2. download cosign $COSIGN_VERSION from github.com/sigstore/cosign and check its SHA-256"
	say "  3. check that checksums.txt was signed by $identity"
	say "  4. check $archive against checksums.txt"
	say "  5. put snugbox, snugboxd and snugbox-helper in $prefix/libexec/snugbox and link $prefix/bin/snugbox, $how"
	say "Nothing else changes, and nothing starts until you run sudo snugbox watch install."
	if [ "$dry_run" = 1 ]; then
		say "Dry run: nothing was downloaded or installed."
		exit 0
	fi
	[ "$elevate" != none ] ||
		die "copying into $prefix needs root, and this system has neither sudo nor doas; run the script as root"

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT INT TERM
	fetch "$base/$archive" "$tmp/$archive"
	fetch "$base/checksums.txt" "$tmp/checksums.txt"
	fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json"
	fetch "https://github.com/sigstore/cosign/releases/download/$COSIGN_VERSION/cosign-linux-$arch" "$tmp/cosign"

	printf '%s  %s\n' "$cosign_sha" "$tmp/cosign" | sha256sum -c - >/dev/null 2>&1 ||
		die "cosign's SHA-256 does not match the pinned one; stopping"
	chmod 0755 "$tmp/cosign"
	"$tmp/cosign" verify-blob \
		--bundle "$tmp/checksums.txt.sigstore.json" \
		--certificate-identity "$identity" \
		--certificate-oidc-issuer https://token.actions.githubusercontent.com \
		"$tmp/checksums.txt" >/dev/null 2>&1 ||
		die "checksums.txt is not signed by $identity; stopping"
	say "The signature of checksums.txt is good."

	line=$(grep "  $archive\$" "$tmp/checksums.txt") || die "checksums.txt does not list $archive; stopping"
	(cd "$tmp" && printf '%s\n' "$line" | sha256sum -c - >/dev/null 2>&1) ||
		die "$archive does not match checksums.txt; stopping"
	say "$archive matches its checksum."

	mkdir "$tmp/x"
	tar -xzf "$tmp/$archive" -C "$tmp/x"
	for bin in snugbox snugboxd snugbox-helper; do
		[ -f "$tmp/x/$bin" ] || die "$archive has no $bin; stopping"
	done
	as_root install -d -m 0755 "$prefix/libexec/snugbox" "$prefix/bin"
	as_root install -m 0755 "$tmp/x/snugbox" "$tmp/x/snugboxd" "$tmp/x/snugbox-helper" "$prefix/libexec/snugbox/"
	as_root ln -sfn ../libexec/snugbox/snugbox "$prefix/bin/snugbox"

	say "Installed Snugbox $tag in $prefix/libexec/snugbox."
	if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet snugboxd.service 2>/dev/null; then
		say "The watcher still runs the version before this one. To update its units and restart it:"
		say "  sudo snugbox watch install"
		return
	fi
	say "See what this server needs, read-only:  sudo snugbox audit"
	say "Keep history and turn on fixes:         sudo snugbox watch install"
}

main "$@"
