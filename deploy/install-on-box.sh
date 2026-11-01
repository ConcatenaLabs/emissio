#!/bin/bash
# One-shot Emissio install for the sequentiatestnet.com box.
# Run as root: bash /root/sequentia/emissio/deploy/install-on-box.sh
set -euo pipefail

cd /root/sequentia/emissio

echo "== build =="
PATH=/root/toolchains/go/bin:$PATH go build .

echo "== user and files =="
# Emissio runs as its own system user, which owns its data and nothing else.
id emissio >/dev/null 2>&1 || useradd --system --user-group --home-dir /var/lib/emissio --shell /usr/sbin/nologin emissio
install -d -m 755 /opt/emissio
install -m 755 emissio /opt/emissio/emissio.new && mv -f /opt/emissio/emissio.new /opt/emissio/emissio
install -d -o emissio -g emissio -m 700 /var/lib/emissio
chown -R emissio:emissio /var/lib/emissio
find /var/lib/emissio -maxdepth 1 -type f -exec chmod 600 {} +
install -d -m 755 /etc/emissio
[ -e /etc/emissio/emissio.env ] || install -m 600 /dev/null /etc/emissio/emissio.env
chmod 600 /etc/emissio/emissio.env

echo "== systemd =="
cp deploy/emissio.service /etc/systemd/system/emissio.service
systemctl daemon-reload
systemctl enable emissio
systemctl restart emissio
sleep 2
systemctl --no-pager --lines=3 status emissio | head -6

echo "== caddy route =="
if ! grep -q "handle_path /emissio" /etc/caddy/Caddyfile; then
    cp /etc/caddy/Caddyfile "/etc/caddy/Caddyfile.bak.$(date +%s)"
    python3 - <<'EOF'
import re
p = "/etc/caddy/Caddyfile"
s = open(p).read()
route = """    redir /emissio /emissio/ permanent
    handle_path /emissio/* {
        reverse_proxy 127.0.0.1:8095
    }
    handle {
        reverse_proxy 127.0.0.1:8080
    }
"""
# Replace the bare catch-all inside the canonical site block.
new = s.replace("sequentiatestnet.com {\n    reverse_proxy 127.0.0.1:8080\n}",
                "sequentiatestnet.com {\n" + route + "}", 1)
if new == s:
    raise SystemExit("Caddyfile did not match the expected canonical block; edit it by hand.")
open(p, "w").write(new)
print("Caddyfile updated")
EOF
    caddy validate --config /etc/caddy/Caddyfile
    systemctl reload caddy
else
    echo "route already present"
fi

echo "== smoke =="
curl -s -o /dev/null -w "local: %{http_code}\n" http://127.0.0.1:8095/emissio/
curl -s -o /dev/null -w "public: %{http_code}\n" https://sequentiatestnet.com/emissio/

echo "== admin account =="
echo "Create the admin from your own machine so the password never lives on this server:"
echo "  openssl rand -hex 12   (keep it in a local password store)"
echo "  printf '%s\n' 'that-password' | ssh root@<box> 'runuser -u emissio -- env EMISSIO_DB=/var/lib/emissio/emissio.db /opt/emissio/emissio createadmin you@example.com'"

echo "== done: https://sequentiatestnet.com/emissio/ =="
