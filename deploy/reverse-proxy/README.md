# reverse-proxy/

Reference Caddy + nginx configs for putting TLS in front of the
read API + api-log-viewer SPA on a single domain.

## What goes where

The api-log backend has two listeners:

| Listener | Default port | Who connects |
|---|---|---|
| **Proxy** | `:7861` | LLM clients (Claude Code, codex, your team's apps). They send their raw `Authorization` / `x-api-key` headers; api-log forwards unchanged to the upstream gateway. |
| **Read API + `/healthz`** | `:7862` | api-log-viewer + ops tools + `/healthz` probes. |

The reverse-proxy sample puts `:7862` behind TLS at
`apilog.example.com/api/*` and `apilog.example.com/healthz`, and
serves the api-log-viewer SPA from the same origin so the viewer
doesn't need CORS.

The proxy listener (`:7861`) is **NOT** put behind TLS. Clients
talk to it directly over your internal network. Exposing it to
the public internet is rarely what you want — clients carry raw
API keys and api-log is not a gateway.

## Caddy

[`Caddyfile`](./Caddyfile) — single-block config. Replace
`apilog.example.com` with your domain and Caddy will provision a
Let's Encrypt cert on first start.

```sh
sudo cp Caddyfile /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

## nginx

[`nginx.conf`](./nginx.conf) — single `server` block. Replace
`server_name`, the cert paths, and (optionally) the upstream
addresses if api-log doesn't run on `127.0.0.1`.

```sh
sudo cp nginx.conf /etc/nginx/sites-available/api-log
sudo ln -sf /etc/nginx/sites-available/api-log /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

## Viewer asset path

Both samples expect the api-log-viewer SPA at
`/opt/api-log-viewer/dist`. To install:

```sh
gh release download v0.1.0 \
  --repo 2nd1st/api-log-viewer \
  --pattern 'dist.zip'
sudo unzip -q dist.zip -d /opt/api-log-viewer
```

If you'd rather not run a separate static-file serve, api-log can
host the viewer itself at `/viewer/` via the built-in hosted-viewer
path (see `APILOG_VIEWER_*` env vars and the top-level README). In
that case, just reverse-proxy `/` straight to `:7862` and skip
the `root` + `try_files` blocks.
