#!/bin/sh
# Boots each supported distribution (blueprint section 9.7) as a throwaway
# KVM virtual machine, copies in the release and test/vm/run.sh, and runs
# the whole life of the agent on it, with a real reboot in the middle of a
# fix. Nothing here touches the host beyond /dev/kvm and a work folder.
#
# It needs a Linux host with KVM (for example WSL2 with /dev/kvm), qemu, the
# cloud-image-utils package for cloud-localds, curl and ssh-keygen. Each
# image is downloaded once, checked against the vendor's own signed or
# published checksum, and cached.
#
#   host.sh <release-dir> [distro ...]
#
# With no distro named it runs them all. The release folder holds the
# tarballs, packages and helper-<arch>.test built by test/vm/prepare.sh.
set -eu

rel=${1:?usage: host.sh <release-dir> [distro ...]}
shift || true
rel=$(cd "$rel" && pwd)
here=$(cd "$(dirname "$0")" && pwd)
cache=${SNUGBOX_VM_CACHE:-$HOME/.cache/snugbox-vm}
work=$(mktemp -d)
mkdir -p "$cache"
trap 'pkill -f "$work" 2>/dev/null || true; rm -rf "$work"' EXIT INT TERM

for t in qemu-system-x86_64 cloud-localds qemu-img curl ssh ssh-keygen sha256sum; do
	command -v "$t" >/dev/null || {
		echo "need $t" >&2
		exit 1
	}
done
if ! { [ -r /dev/kvm ] && [ -w /dev/kvm ]; }; then
	echo "no usable /dev/kvm; this test needs KVM" >&2
	exit 1
fi

# spec_for prints "name|checksum-url|image-name-pattern|algo|login" for a
# distribution of blueprint section 9.7. The image is the one the checksum
# file names that matches the pattern, fetched from the folder the checksum
# file was finally served from, so a versioned or mirrored image is found
# too. amd64 only: KVM runs guests of the host's own architecture.
spec_for() {
	case "$1" in
	ubuntu2204) echo "ubuntu-22.04|https://cloud-images.ubuntu.com/releases/22.04/release/SHA256SUMS|ubuntu-22\.04-server-cloudimg-amd64\.img|sha256|ubuntu" ;;
	ubuntu2404) echo "ubuntu-24.04|https://cloud-images.ubuntu.com/releases/24.04/release/SHA256SUMS|ubuntu-24\.04-server-cloudimg-amd64\.img|sha256|ubuntu" ;;
	ubuntu2604) echo "ubuntu-26.04|https://cloud-images.ubuntu.com/releases/26.04/release/SHA256SUMS|ubuntu-26\.04-server-cloudimg-amd64\.img|sha256|ubuntu" ;;
	debian12) echo "debian-12|https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS|debian-12-genericcloud-amd64\.qcow2|sha512|debian" ;;
	debian13) echo "debian-13|https://cloud.debian.org/images/cloud/trixie/latest/SHA512SUMS|debian-13-genericcloud-amd64\.qcow2|sha512|debian" ;;
	alma9) echo "almalinux-9|https://repo.almalinux.org/almalinux/9/cloud/x86_64/images/CHECKSUM|AlmaLinux-9-GenericCloud-latest\.x86_64\.qcow2|sha256|almalinux" ;;
	alma10) echo "almalinux-10|https://repo.almalinux.org/almalinux/10/cloud/x86_64/images/CHECKSUM|AlmaLinux-10-GenericCloud-latest\.x86_64\.qcow2|sha256|almalinux" ;;
	rocky9) echo "rocky-9|https://dl.rockylinux.org/pub/rocky/9/images/x86_64/CHECKSUM|Rocky-9-GenericCloud-Base\.latest\.x86_64\.qcow2|sha256|rocky" ;;
	rocky10) echo "rocky-10|https://dl.rockylinux.org/pub/rocky/10/images/x86_64/CHECKSUM|Rocky-10-GenericCloud-Base\.latest\.x86_64\.qcow2|sha256|rocky" ;;
	fedora) echo "fedora-44|https://download.fedoraproject.org/pub/fedora/linux/releases/44/Cloud/x86_64/images/Fedora-Cloud-44-1.7-x86_64-CHECKSUM|Fedora-Cloud-Base-Generic-44-1\.7\.x86_64\.qcow2|sha256|fedora" ;;
	amazon) echo "amazonlinux-2023|https://cdn.amazonlinux.com/al2023/os-images/latest/kvm/SHA256SUMS|al2023-kvm-.*-kernel-6\.1-x86_64\.xfs\.gpt\.qcow2|sha256|ec2-user" ;;
	*)
		echo "unknown distribution $1" >&2
		return 1
		;;
	esac
}

all="ubuntu2204 ubuntu2404 ubuntu2604 debian12 debian13 alma9 alma10 rocky9 rocky10 fedora amazon"
[ $# -gt 0 ] && all="$*"

ssh_key="$work/id"
ssh-keygen -q -t ed25519 -N "" -f "$ssh_key" -C snugbox-vm
pub=$(cat "$ssh_key.pub")

# fetch downloads $1 to $2 over HTTPS, resuming after a dropped or stalled
# connection, which vendor mirrors have, up to five tries.
fetch() {
	n=0
	until curl -fsSL --proto '=https' --speed-limit 1024 --speed-time 60 -C - -o "$2" "$1"; do
		n=$((n + 1))
		[ "$n" -lt 5 ] || return 1
		echo "retrying $1 ($n of 4)" >&2
		sleep 5
	done
}

# verify_download prints the cached path of the image a checksum file names,
# downloading it first when the cache holds none with that checksum. Both
# the GNU form ("<sum>  <name>") and the BSD form ("SHA256 (<name>) = <sum>")
# are read. The checksum file comes over HTTPS from the vendor's own host or
# its mirror redirector; its signature isn't checked, which a test of
# throwaway machines can live with.
verify_download() {
	sums_url=$1 pattern=$2 algo=$3
	eff=$(curl -fsSL --proto '=https' --retry 3 -o "$work/sums" -w '%{url_effective}' "$sums_url") || return 1
	# The pattern goes in through the environment, where awk doesn't read
	# backslashes as escapes.
	line=$(PAT="^($pattern)\$" awk '
		/^SHA(256|512) \(/ { n = $2; gsub(/[()]/, "", n); if (n ~ ENVIRON["PAT"]) { print n, $4; exit } next }
		NF == 2 && $1 ~ /^[0-9a-fA-F]+$/ { n = $2; sub(/^\*/, "", n); if (n ~ ENVIRON["PAT"]) { print n, $1; exit } }
	' "$work/sums")
	[ -n "$line" ] || {
		echo "no image matching $pattern in $sums_url" >&2
		return 1
	}
	name=${line% *} want=${line#* }
	dst="$cache/$name"
	if [ -f "$dst" ] && [ "$(cat "$dst.ok" 2>/dev/null)" = "$want" ]; then
		echo "$dst"
		return
	fi
	# A part an earlier run left may be complete, which a resumed download
	# fails on, or part of an older build, which fails the checksum; either
	# way the second try starts from nothing.
	got=""
	for try in resume fresh; do
		[ "$try" = resume ] || rm -f "$dst.tmp"
		if fetch "${eff%/*}/$name" "$dst.tmp"; then
			got=$("${algo}sum" "$dst.tmp" | awk '{print $1}')
			[ "$got" = "$want" ] && break
		fi
	done
	[ "$got" = "$want" ] || {
		echo "$name could not be fetched with checksum $want (last got ${got:-nothing})" >&2
		rm -f "$dst.tmp"
		return 1
	}
	mv "$dst.tmp" "$dst"
	echo "$want" >"$dst.ok"
	echo "$dst"
}

# seed builds a cloud-init image that adds our key to the login user and
# lets it sudo without a password.
seed() {
	user=$1 out=$2
	cat >"$work/user-data" <<EOF
#cloud-config
users:
  - name: $user
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/sh
    ssh_authorized_keys:
      - $pub
ssh_pwauth: false
EOF
	cloud-localds "$out" "$work/user-data"
}

summary=""
run_one() {
	spec=$1
	name=$(echo "$spec" | cut -d'|' -f1)
	sums=$(echo "$spec" | cut -d'|' -f2)
	pattern=$(echo "$spec" | cut -d'|' -f3)
	algo=$(echo "$spec" | cut -d'|' -f4)
	user=$(echo "$spec" | cut -d'|' -f5)
	echo "===== $name"
	base=""
	if ! base=$(verify_download "$sums" "$pattern" "$algo"); then
		echo "SKIP $name: no image could be fetched and checked"
		summary="$summary\n$name SKIP (image unavailable)"
		return
	fi

	disk="$work/$name.qcow2"
	# The overlay gets 20 GiB, or the image's own size when that is larger:
	# a smaller overlay cuts off the end of the disk, and with it the
	# partitions of Amazon Linux's 25 GiB image.
	size=$(qemu-img info "$base" | sed -n 's/^virtual size: .*(\([0-9]*\) bytes)$/\1/p')
	[ "${size:-0}" -ge 21474836480 ] || size=21474836480
	qemu-img create -q -f qcow2 -b "$base" -F qcow2 "$disk" "$size"
	seed_img="$work/$name-seed.iso"
	seed "$user" "$seed_img"
	# Pick a port the host is not already listening on, so a leftover VM
	# from an earlier run can't steal this one's forward.
	port=$((2200 + $(echo "$name" | cksum | cut -d' ' -f1) % 300))
	while ss -ltn 2>/dev/null | grep -q ":$port "; do
		port=$((port + 1))
	done

	start_vm() {
		qemu-system-x86_64 -enable-kvm -machine q35 -cpu host -smp 2 -m 2048 \
			-nographic -monitor none \
			-serial "file:$work/$name.console.log" \
			-drive file="$disk",if=virtio,format=qcow2 \
			-drive file="$seed_img",if=virtio,format=raw \
			-netdev user,id=n0,hostfwd=tcp:127.0.0.1:$port-:22 \
			-device virtio-net-pci,netdev=n0 \
			>"$work/$name.qemu.log" 2>&1 &
		echo $!
	}
	ssh_vm() { ssh -i "$ssh_key" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o LogLevel=ERROR "$user@127.0.0.1" "$@"; }
	scp_vm() { scp -i "$ssh_key" -P "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -q "$@"; }
	wait_ssh() {
		i=0
		while [ "$i" -lt 60 ]; do
			ssh_vm true 2>/dev/null && return 0
			i=$((i + 1))
			sleep 5
		done
		return 1
	}

	vm=$(start_vm)
	if ! wait_ssh; then
		echo "FAIL $name: no SSH after 5 minutes; the console said:"
		tail -n 15 "$work/$name.console.log"
		kill "$vm" 2>/dev/null || true
		summary="$summary\n$name FAIL (no boot)"
		return
	fi

	ssh_vm "sudo mkdir -p /opt/snugrel /opt/snugtest"
	for f in "$rel"/snugbox_*_linux_amd64.tar.gz "$rel"/snugbox_*_amd64.deb "$rel"/snugbox-*.x86_64.rpm "$rel"/helper-amd64.test; do
		[ -f "$f" ] && scp_vm "$f" "$user@127.0.0.1:/tmp/" && ssh_vm "sudo mv /tmp/$(basename "$f") /opt/snugrel/"
	done
	scp_vm "$here/run.sh" "$user@127.0.0.1:/tmp/vm-run.sh"
	scp_vm "$here/../autoundo/run.sh" "$user@127.0.0.1:/tmp/vm-autoundo.sh"
	ssh_vm "sudo mv /tmp/vm-run.sh /opt/snugtest/run.sh && sudo mv /tmp/vm-autoundo.sh /opt/snugtest/autoundo.sh"

	ok=1
	before() { ssh_vm "sudo sh /opt/snugtest/run.sh before /opt/snugrel 2>&1" | tee "$work/$name.before.log"; }
	reboot_vm() {
		echo "--- $name rebooting $1"
		ssh_vm "sudo reboot" 2>/dev/null || true
		sleep 20
		wait_ssh
	}
	before
	if grep -q '^REBOOT FOR PSI' "$work/$name.before.log"; then
		if reboot_vm "to turn on pressure stall information"; then before; else : >"$work/$name.before.log"; fi
	fi
	if grep -q 'all passed' "$work/$name.before.log"; then
		if reboot_vm "in the middle of a fix"; then
			ssh_vm "sudo sh /opt/snugtest/run.sh after /opt/snugrel 2>&1" | tee "$work/$name.after.log" || ok=0
			grep -q 'all passed' "$work/$name.after.log" || ok=0
		else
			echo "FAIL $name: no SSH after the reboot"
			ok=0
		fi
	else
		ok=0
	fi

	ssh_vm "sudo poweroff" 2>/dev/null || true
	sleep 3
	kill "$vm" 2>/dev/null || true
	wait "$vm" 2>/dev/null || true
	# Wait for the forwarded port to free before the next distribution.
	j=0
	while ss -ltn 2>/dev/null | grep -q ":$port " && [ "$j" -lt 10 ]; do
		sleep 1
		j=$((j + 1))
	done
	if [ "$ok" = 1 ]; then
		echo "PASS $name"
		summary="$summary\n$name PASS"
	else
		echo "FAIL $name"
		summary="$summary\n$name FAIL"
	fi
}

for d in $all; do
	spec=$(spec_for "$d") || exit 2
	run_one "$spec"
done

echo "====="
printf '%b\n' "$summary"
printf '%b' "$summary" | grep -q FAIL && exit 1
exit 0
