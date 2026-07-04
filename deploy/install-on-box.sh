#!/bin/bash
# One-shot Emissio install for the sequentiatestnet.com box.
# Run as root: bash /root/sequentia/emissio/deploy/install-on-box.sh
set -euo pipefail

cd /root/sequentia/emissio

echo "== build =="
PATH=/root/toolchains/go/bin:$PATH go build .

echo "== systemd =="
mkdir -p /var/lib/emissio
cp deploy/emissio.service /etc/systemd/system/emissio.service
systemctl daemon-reload
systemctl enable --now emissio
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
if [ ! -f /root/emissio-admin.txt ]; then
    PW=$(openssl rand -hex 12)
    printf 'emissio admin\nemail: aejkohl@gmail.com\npassword: %s\n' "$PW" > /root/emissio-admin.txt
    chmod 600 /root/emissio-admin.txt
    echo "$PW" | EMISSIO_DB=/var/lib/emissio/emissio.db ./emissio createadmin aejkohl@gmail.com
    echo "admin created; credentials in /root/emissio-admin.txt (box only, do not commit)"
else
    echo "admin credentials already exist at /root/emissio-admin.txt"
fi

echo "== done: https://sequentiatestnet.com/emissio/ =="
