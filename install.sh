#!/bin/sh
# Installs hmj from a GitHub release, or updates it: run it again for the latest release.
#
#   curl -fsSL https://raw.githubusercontent.com/himanshu-mahendra-jain/hmj-agent/main/install.sh | sh
#
# HMJ_VERSION picks a release (such as v1.2.3) instead of the latest one, and HMJ_INSTALL_DIR the
# directory the binary goes to (default ~/.local/bin). HMJ_REPO_URL points at another copy of the
# repository, such as a mirror; the tests use it for a local server.
set -eu

repo_url=${HMJ_REPO_URL:-https://github.com/himanshu-mahendra-jain/hmj-agent}
install_dir=${HMJ_INSTALL_DIR:-$HOME/.local/bin}
version=${HMJ_VERSION:-}

fail() {
	echo "install.sh: $*" >&2
	exit 1
}

for tool in curl tar sha256sum; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done

# Releases are built for Linux on amd64 and arm64 only.
[ "$(uname -s)" = Linux ] || fail "hmj runs on Linux only"
case $(uname -m) in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) fail "no release is built for $(uname -m); build from source with make build" ;;
esac

# The latest release is where /releases/latest redirects, which needs neither the GitHub API
# nor its rate limit. Draft releases are not counted until they are published.
if [ -z "$version" ]; then
	latest=$(curl -fsSIL -o /dev/null -w '%{url_effective}' "$repo_url/releases/latest") ||
		fail "found no published release at $repo_url/releases"
	version=${latest##*/}
fi
case $version in v*) ;; *) version=v$version ;; esac
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' ||
	fail "found no published release at $repo_url/releases (got '$version')"

current=$("$install_dir/hmj" --version 2>/dev/null || true)
case $current in
"HMJ Agent ${version#v} ("*)
	echo "hmj $version is already installed in $install_dir"
	exit 0
	;;
esac

tmp=$(mktemp -d)
staged=
trap 'rm -rf "$tmp"; [ -z "$staged" ] || rm -f "$staged"' EXIT
trap 'exit 1' HUP INT TERM

tarball=hmj-${version#v}-linux-$arch.tar.gz
base=$repo_url/releases/download/$version
echo "Downloading hmj $version for linux/$arch"
curl -fsSL -o "$tmp/$tarball" "$base/$tarball" || fail "cannot download $base/$tarball"
curl -fsSL -o "$tmp/SHA256SUMS.txt" "$base/SHA256SUMS.txt" || fail "cannot download $base/SHA256SUMS.txt"

# Guards against a broken or truncated download; the sums come from the same release.
awk -v name="$tarball" '$2 == name' "$tmp/SHA256SUMS.txt" >"$tmp/sum.txt"
[ -s "$tmp/sum.txt" ] || fail "SHA256SUMS.txt has no line for $tarball"
(cd "$tmp" && sha256sum -c --status sum.txt) || fail "$tarball does not match its SHA-256 sum"

tar -xzf "$tmp/$tarball" -C "$tmp" hmj/hmj || fail "$tarball holds no hmj/hmj"

# Copied beside the old binary and renamed over it, so a running hmj keeps its file.
mkdir -p "$install_dir"
staged=$install_dir/.hmj.new.$$
cp "$tmp/hmj/hmj" "$staged"
chmod 0755 "$staged"
mv -f "$staged" "$install_dir/hmj"
staged=

if [ -n "$current" ]; then
	echo "Updated $install_dir/hmj from: $current"
else
	echo "Installed $install_dir/hmj"
fi
"$install_dir/hmj" --version
case :$PATH: in
*:"$install_dir":*) ;;
*) echo "$install_dir is not on PATH; add it with: export PATH=\"$install_dir:\$PATH\"" ;;
esac
