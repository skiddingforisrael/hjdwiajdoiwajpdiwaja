#!/bin/sh
set -eu

if [ "$(id -u)" -ne 0 ]; then
    exec sudo "$0" "$@"
fi

repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cone_version=${CONE_VERSION:-1.5.1}
case "$(uname -m)" in
    aarch64|arm64)
        release_target=arm64
        ;;
    armv7l|armv7*)
        release_target=armv7
        ;;
    *)
        echo "Unsupported Raspberry Pi architecture: $(uname -m)" >&2
        exit 1
        ;;
esac

for command in curl sha256sum tar nginx; do
    if ! command -v "$command" >/dev/null 2>&1; then
        echo "$command is required for the Cone Pi deployment." >&2
        echo "Install dependencies with: sudo apt-get update && sudo apt-get install -y nginx curl" >&2
        exit 1
    fi
done

archive="cone-v${cone_version}-linux-${release_target}.tar.gz"
release_url="https://github.com/qaustria/AutoPack-Go/releases/download/v${cone_version}"
release_dir=$(mktemp -d /tmp/cone-release.XXXXXX)
trap 'rm -rf -- "$release_dir"' EXIT HUP INT TERM
curl -fsSL "$release_url/$archive" -o "$release_dir/$archive"
curl -fsSL "$release_url/SHA256SUMS" -o "$release_dir/SHA256SUMS"
checksum=$(awk -v archive="$archive" '$2 == "./" archive || $2 == archive { print; exit }' "$release_dir/SHA256SUMS")
if [ -z "$checksum" ]; then
    echo "Release checksum is missing for $archive." >&2
    exit 1
fi
printf '%s\n' "$checksum" | (cd "$release_dir" && sha256sum -c -)
tar -xzf "$release_dir/$archive" -C "$release_dir"
source_binary="$release_dir/cone-v${cone_version}-linux-${release_target}/cone"
if [ ! -x "$source_binary" ]; then
    echo "Release archive does not contain an executable Cone binary." >&2
    exit 1
fi

nginx_limits=/etc/nginx/conf.d/cone-limits.conf
nginx_site=/etc/nginx/sites-available/qstr.xyz
nginx_limits_backup=""
nginx_site_backup=""
if [ -f "$nginx_limits" ]; then
    nginx_limits_backup=$(mktemp /tmp/cone-nginx-limits-backup.XXXXXX)
    cp "$nginx_limits" "$nginx_limits_backup"
fi
if [ -f "$nginx_site" ]; then
    nginx_site_backup=$(mktemp /tmp/cone-nginx-site-backup.XXXXXX)
    cp "$nginx_site" "$nginx_site_backup"
fi
install -d -m 0755 /etc/nginx/conf.d
install -d -m 0755 /etc/nginx/sites-available /etc/nginx/sites-enabled
install -m 0644 "$repo_dir/deploy/nginx/cone-limits.conf" "$nginx_limits"
install -m 0644 "$repo_dir/deploy/nginx/qstr.xyz" "$nginx_site"
ln -sfn "$nginx_site" /etc/nginx/sites-enabled/qstr.xyz
if ! nginx -t; then
    if [ -n "$nginx_limits_backup" ]; then
        install -m 0644 "$nginx_limits_backup" "$nginx_limits"
    else
        rm -f "$nginx_limits"
    fi
    if [ -n "$nginx_site_backup" ]; then
        install -m 0644 "$nginx_site_backup" "$nginx_site"
    else
        rm -f "$nginx_site" /etc/nginx/sites-enabled/qstr.xyz
    fi
    rm -f "$nginx_limits_backup" "$nginx_site_backup"
    echo "Cone nginx configuration failed validation and was rolled back." >&2
    exit 1
fi
rm -f "$nginx_limits_backup" "$nginx_site_backup"

# Older installations may also have a per-user Cone unit. Stop and disable it
# before starting the system unit so an outdated process cannot retain port 8080.
service_user=$(systemctl show cone -p User --value 2>/dev/null || true)
if [ -n "$service_user" ] && service_uid=$(id -u "$service_user" 2>/dev/null); then
    runuser -u "$service_user" -- env XDG_RUNTIME_DIR="/run/user/$service_uid" \
        systemctl --user disable --now cone.service >/dev/null 2>&1 || true
fi

systemctl stop cone
install -m 0755 "$source_binary" "$repo_dir/cone"
install -d -m 0755 /etc/systemd/system/cone.service.d
install -m 0644 "$repo_dir/deploy/systemd/cone-pi.conf" /etc/systemd/system/cone.service.d/20-cone-network.conf
systemctl daemon-reload
systemctl start cone

i=0
while [ "$i" -lt 20 ]; do
    if health=$(curl -fsS http://127.0.0.1:8080/healthz 2>/dev/null); then
        printf '%s\n' "$health"
        case "$health" in
            *'"version":"'"$cone_version"'"'*) break ;;
        esac
    fi
    i=$((i + 1))
    sleep 1
done

if [ "$i" -ge 20 ]; then
    echo "Cone started, but its private healthz did not report version $cone_version." >&2
    journalctl -u cone -n 30 --no-pager >&2 || true
    exit 1
fi

systemctl enable nginx >/dev/null
systemctl restart nginx
health=$(curl -fsS -H 'Host: qstr.xyz' http://127.0.0.1/healthz)
printf '%s\n' "$health"
case "$health" in
    *'"version":"'"$cone_version"'"'*) ;;
    *)
        echo "nginx is running, but the proxied healthz did not report version $cone_version." >&2
        journalctl -u nginx -n 30 --no-pager >&2 || true
        exit 1
        ;;
esac

echo "Cone $cone_version is running through nginx on localhost:80."
