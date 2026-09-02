#!/bin/sh
# Install newt on a reMarkable tablet over ssh and make it survive reboots and
# firmware updates. Run from a computer with the tablet plugged in.
#
#   remarkable/install.sh --id ID --secret SECRET --endpoint https://pangolin.example
#   remarkable/install.sh                       # keep the config already on the tablet
#   HOST=root@192.168.1.20 remarkable/install.sh
#
# Files on the tablet:
#   /home/root/newt/newt                the binary for its architecture
#   /home/root/newt/config.json         id, secret, endpoint
#   /home/root/units/newt.service       the unit, staged by rm-persist
#   /home/root/.local/bin/rm-persist    persistence tool
set -eu
cd "$(dirname "$0")"
HOST=${HOST:-root@10.11.99.1}
ID= SECRET= ENDPOINT=
while [ $# -gt 0 ]; do
	case $1 in
	--id) ID=$2; shift ;;
	--secret) SECRET=$2; shift ;;
	--endpoint) ENDPOINT=$2; shift ;;
	*) echo "unknown option $1" >&2; exit 2 ;;
	esac
	shift
done

run() { ssh "$HOST" "$@"; }
push() { ssh "$HOST" "cat > '$2'" < "$1"; }

arch=$(run uname -m)
case $arch in
aarch64) bin=out/newt_linux_arm64 ;;
armv7l | armv6l) bin=out/newt_linux_arm32 ;;
*) echo "unsupported architecture $arch" >&2; exit 1 ;;
esac
[ -f "$bin" ] || { echo "$bin missing, run remarkable/build.sh first" >&2; exit 1; }

echo "tablet: $(run 'cat /sys/devices/soc0/machine 2>/dev/null || echo reMarkable') $arch, firmware $(run cat /etc/version)"

run "systemctl stop newt.service 2>/dev/null || true"
run "mkdir -p /home/root/newt /home/root/units /home/root/.local/bin"
push "$bin" /home/root/newt/newt
push newt.service /home/root/units/newt.service
push rm-persist /home/root/.local/bin/rm-persist
run "chmod 755 /home/root/newt/newt /home/root/.local/bin/rm-persist"
if [ "$(md5sum < "$bin" | cut -c1-32)" != "$(run md5sum /home/root/newt/newt | cut -c1-32)" ]; then
	echo "checksum mismatch after upload" >&2
	exit 1
fi

if [ -n "$ID" ] || [ -n "$SECRET" ] || [ -n "$ENDPOINT" ]; then
	[ -n "$ID" ] && [ -n "$SECRET" ] && [ -n "$ENDPOINT" ] || { echo "--id, --secret and --endpoint go together" >&2; exit 2; }
	printf '{\n  "id": "%s",\n  "secret": "%s",\n  "endpoint": "%s"\n}\n' "$ID" "$SECRET" "$ENDPOINT" | run "umask 077; cat > /home/root/newt/config.json"
elif ! run "test -s /home/root/newt/config.json"; then
	if run "test -s /home/root/.config/newt-client/config.json"; then
		run "cp /home/root/.config/newt-client/config.json /home/root/newt/config.json; chmod 600 /home/root/newt/config.json"
		echo "reused /home/root/.config/newt-client/config.json"
	else
		echo "no config on the tablet, pass --id --secret --endpoint" >&2
		exit 2
	fi
fi
run "chmod 600 /home/root/newt/config.json"

run "/home/root/.local/bin/rm-persist install"
run "systemctl restart newt.service"
sleep 3
run "systemctl --no-pager status newt.service | head -n 12"
run "/home/root/.local/bin/rm-persist status"
