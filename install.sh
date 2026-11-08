#!/bin/sh
# Install mekiki from GitHub Releases on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/Mimic890/mekiki/main/install.sh | sh
#   curl -fsSL .../install.sh | VERSION=v0.2.2 sh    # a specific version
#
# The archive is checked against the release's SHA256SUMS. Running it again
# replaces the binary. FFmpeg and ExifTool are installed only after asking.
set -eu

REPO=Mimic890/mekiki
BINDIR=/usr/local/bin
VERSION=${VERSION:-latest}
README="https://github.com/$REPO#install"

say() { printf '%s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }
has() { command -v "$1" >/dev/null 2>&1; }

# Opening /dev/tty fails without a controlling terminal, even if the file exists.
has_tty() { (: </dev/tty) 2>/dev/null; }

fetch() {
	if has curl; then
		curl -fsSL --retry 3 -o "$2" "$1"
	elif has wget; then
		wget -q -O "$2" "$1"
	else
		die "curl or wget is required"
	fi
}

sha256_check() {
	if has sha256sum; then
		sha256sum -c - >/dev/null
	elif has shasum; then
		shasum -a 256 -c - >/dev/null
	else
		die "sha256sum or shasum is required"
	fi
}

install_binary() {
	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported system: $(uname -s)" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac

	if [ "$VERSION" = latest ]; then
		base="https://github.com/$REPO/releases/latest/download"
	else
		base="https://github.com/$REPO/releases/download/$VERSION"
	fi

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 130' INT TERM

	fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
	archive=$(awk -v re="^mekiki_[^/]*_${os}_${arch}[.]tar[.]gz\$" '$2 ~ re { print $2; exit }' "$tmp/SHA256SUMS")
	[ -n "$archive" ] || die "no archive for $os/$arch in $base/SHA256SUMS"

	say "Downloading $archive..."
	fetch "$base/$archive" "$tmp/$archive" || die "download failed: $base/$archive"
	(cd "$tmp" && awk -v a="$archive" '$2 == a' SHA256SUMS | sha256_check) ||
		die "checksum mismatch for $archive"
	say "Checksum OK."

	tar -xzf "$tmp/$archive" -C "$tmp" "${archive%.tar.gz}/mekiki"

	run=
	if [ ! -w "$BINDIR" ] && [ "$(id -u)" != 0 ]; then
		has sudo || die "cannot write to $BINDIR and sudo is not available"
		run=sudo
	fi
	$run mkdir -p "$BINDIR" || die "cannot create $BINDIR"
	$run install -m 0755 "$tmp/${archive%.tar.gz}/mekiki" "$BINDIR/mekiki" || die "cannot write to $BINDIR"
	say "Installed $("$BINDIR/mekiki" --version) to $BINDIR/mekiki"
}

# install_dependencies offers to install what is missing. It never installs
# without a yes, and never enables a third-party repository.
install_dependencies() {
	need_ffmpeg='' need_exif=''
	has ffmpeg && has ffprobe || need_ffmpeg=1
	has exiftool || need_exif=1
	[ -n "$need_ffmpeg$need_exif" ] || return 0

	sudo="sudo "
	[ "$(id -u)" != 0 ] || sudo=
	pkgs=${need_ffmpeg:+ffmpeg }
	if [ "$(uname -s)" = Darwin ]; then
		pkgs=$pkgs${need_exif:+exiftool}
		if ! has brew; then
			say "Install Homebrew from https://brew.sh, then run: brew install $pkgs"
			return 0
		fi
		cmd="brew install $pkgs"
	elif has apt-get; then
		pkgs=$pkgs${need_exif:+libimage-exiftool-perl}
		cmd="${sudo}apt-get update && ${sudo}apt-get install -y $pkgs"
	elif has pacman; then
		pkgs=$pkgs${need_exif:+perl-image-exiftool}
		cmd="${sudo}pacman -S --needed --noconfirm $pkgs"
	elif has dnf && grep -q '^ID=fedora' /etc/os-release 2>/dev/null; then
		say "FFmpeg on Fedora comes from RPM Fusion, which this script does not enable. Run:"
		if [ -n "$need_ffmpeg" ]; then
			say "  ${sudo}dnf install https://mirrors.rpmfusion.org/free/fedora/rpmfusion-free-release-\$(rpm -E %fedora).noarch.rpm"
			say "  ${sudo}dnf install --allowerasing ffmpeg"
		fi
		[ -z "$need_exif" ] || say "  ${sudo}dnf install perl-Image-ExifTool"
		return 0
	else
		say "FFmpeg or ExifTool is missing and no supported package manager was found. See $README"
		return 0
	fi

	if ! has_tty; then
		say "FFmpeg or ExifTool is missing. No terminal to ask, so nothing was installed. Run:"
		say "  $cmd"
		return 0
	fi
	printf 'FFmpeg or ExifTool is missing. Run "%s"? [y/N] ' "$cmd" >/dev/tty
	reply=
	read -r reply </dev/tty || reply=
	case $reply in
	y | Y | yes | YES) sh -c "$cmd" || say "Could not install them. Run it yourself: $cmd" ;;
	*) say "Skipped. To install them later: $cmd" ;;
	esac
}

# Everything runs from here, so a truncated download executes nothing.
main() {
	PATH=$PATH:/usr/bin/vendor_perl # where Arch puts exiftool; only login shells have it
	install_binary
	install_dependencies
	"$BINDIR/mekiki" doctor || true # exits with 4 while a required tool is missing
}

main
