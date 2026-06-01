# systemd

Reference unit for running api-log as a native binary under
systemd. Use this when you don't want Docker — common for sub2api /
CLIProxyAPI / new-api operators running on a homelab box, a small
VPS, or anywhere `docker` would just add a moving part.

## One-time setup

```sh
# 1. Install the binary
go install github.com/2nd1st/api-log/cmd/api-log@v0.1.0
sudo install -m 0755 "$(go env GOPATH)/bin/api-log" /usr/local/bin/

# 2. Create the service user + data dir
sudo useradd --system --home /var/lib/api-log --shell /usr/sbin/nologin api-log
sudo mkdir -p /var/lib/api-log /etc/api-log
sudo chown api-log:api-log /var/lib/api-log

# 3. Drop an env file (edit to match your upstream gateway)
sudo tee /etc/api-log/env > /dev/null <<'EOF'
APILOG_PROXY_LISTEN=0.0.0.0:7861
APILOG_PROXY_UPSTREAM=http://127.0.0.1:7860
APILOG_API_LISTEN=127.0.0.1:7862
APILOG_STORAGE_DATA_DIR=/var/lib/api-log/data
EOF

# 4. Install + start
sudo cp api-log.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now api-log
```

## Verify

```sh
systemctl status api-log
curl -s http://127.0.0.1:7862/healthz | jq .
```

The admin bearer token is auto-generated on first run at
`/var/lib/api-log/data/admin_token`.

## Updating

Re-run `go install` to refresh the binary, then
`sudo install -m 0755 ... /usr/local/bin/` and
`sudo systemctl restart api-log`.

## Reverse proxy

The proxy listener (`:7861`) is what your clients hit; the read
API (`:7862`) is what `api-log-viewer` and the `/healthz` check
read. See [`../reverse-proxy/`](../reverse-proxy/) for nginx +
Caddy samples that put TLS in front of the read API.
