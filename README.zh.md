[English](README.md) | 中文

# api-log: LLM proxy logging and API trace recorder

[![CI](https://github.com/xiayangzhang/api-log/actions/workflows/ci.yml/badge.svg)](https://github.com/xiayangzhang/api-log/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/xiayangzhang/api-log?include_prereleases&sort=semver)](https://github.com/xiayangzhang/api-log/releases)
[![Go version](https://img.shields.io/github/go-mod/go-version/xiayangzhang/api-log)](./go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](./LICENSE)

api-log 是一个面向 LLM 网关可观测性的透明 HTTP 录制 proxy。它位于客户端与 OpenAI-compatible 或 Anthropic Messages 网关之间，原样转发流量，将每条完成的请求/响应 trace 以 append-only JSONL 落盘，并构建一份 SQLite 索引供本地检索、重放与分析。

转发 goroutine 不解析任何 body。JSON 反序列化、SSE 事件切分、session 推断都在响应回写给客户端之后的 finalize 阶段进行。Token 计费、评测流水线、以及任何语义层面的解释都在本项目范围之外。

## Status

正在准备 `v0.1.0` tag。capture 路径、read API、viewer（独立仓库）、以及 plugin 系统（Phase A observer + Phase B/C mutator）均已落地并跑在真实流量上。HTTP read API 契约已稳定；tag 之前的 commit 仍可能 rebase。

磁盘格式与 read API 契约见 [ARCHITECTURE.md](./ARCHITECTURE.md)，排期见 [ROADMAP.md](./ROADMAP.md)。

## vs alternatives

| 工具 | 是什么 | 为什么 api-log 不一样 |
|---|---|---|
| Helicone | 网关 / 可观测性整套方案 | api-log 只做透明录制；不路由、不计费、不鉴权，也不是托管服务。 |
| Langfuse | 应用层 LLM tracing 平台 | api-log 在网关边界抓 HTTP 流量，无需 SDK 埋点。 |
| Phoenix | 评测 / tracing / 可观测性工具集 | api-log 先把原始网关 trace 录下来；评测流水线是下游环节。 |
| LangSmith | LangChain 的 tracing / 评测平台 | api-log 与框架无关，本地落 JSONL + SQLite。 |
| mitmproxy | 通用交互式 proxy | api-log 理解 LLM 的 JSON/SSE envelope，写结构化 trace。 |

api-log 不是网关。它不鉴权、不路由、不重试、不限流、不缓存、不改写——这些都留给上游网关。

## Quick start

### Docker Compose

```yaml
# docker-compose.yml — added alongside your existing gateway
services:
  gateway:                                  # CPA / sub2api / new-api / your stack
    # ... existing config ...
    expose: ["7860"]                        # move 7860 from "ports" to "expose"

  api-log:
    image: ghcr.io/xiayangzhang/api-log:latest
    ports:
      - "7861:7861"                         # proxy listener (clients connect here)
      - "7862:7862"                         # read API
    environment:
      APILOG_PROXY_UPSTREAM: http://gateway:7860
    volumes:
      - ./api-log-data:/data
```

```bash
docker compose up -d
```

[`deploy/`](./deploy/README.md) 下有三套参考栈——`dev-stack/`（api-log + mock LLM 网关，无需真实上游）、`demo/`（api-log 挡在 `sub2api` 前面）、`bench/`（api-log 单跑，上游 URL 通过 env 传入）。想 5 分钟跑通就用 `deploy/dev-stack/`；[`tests/integration/run.sh`](./tests/integration/run.sh) 也是跑这套。

### Point clients at api-log

把客户端的 `base_url` 从网关端口（`:7860`）改到 api-log 的 proxy listener（`:7861`）。客户端其他配置都不动。

### Verify a captured trace

```bash
# 1. Liveness — unauthenticated, k8s probe compatible
curl -s http://localhost:7862/healthz | jq .

# 2. Send one request through the proxy (replace with a real client call)
curl -s http://localhost:7861/v1/messages \
  -H "x-api-key: $UPSTREAM_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}'

# 3. Read the auto-generated admin bearer
TOKEN=$(cat ./api-log-data/admin_token)

# 4. List recent traces from the read API
curl -s -H "Authorization: Bearer $TOKEN" \
  'http://localhost:7862/api/traces?limit=5' | jq '.traces[] | {id, path, status, model}'
```

admin bearer 在首次启动时生成，写入 `data/admin_token`。删掉该文件并重启即可轮换。

## How recording works

### Traffic path

```
client(s)  →  api-log  →  CPA / sub2api / new-api / any OpenAI-compatible gateway  →  upstream
                ↓
          data/<date>/<key_hash>.jsonl    (append-only, source of truth)
          data/index.sqlite               (derived index, rebuildable)
```

proxy listener 接受 plain HTTP。转发用 `httputil.ReverseProxy` 加自定义 `Transport`，把请求与响应 body 同时 tee 到 per-trace 临时文件。响应结束（成功、客户端断连、或上游报错）后，finalize 阶段把 body 解析成 JSONL 行，写入当天的 JSONL 文件，并把索引列插入 SQLite。完整写路径见 [ARCHITECTURE § 7](./ARCHITECTURE.md)。

### Storage model

两层，权威次序严格：

1. **`data/<date>/<key_hash>.jsonl`** —— 每条完成 trace 一行。按客户端 key 分文件、按日分目录，append-only。每行携带完整 HTTP 事务（请求 header + body，响应 header + body 或流式的 `events[]`，时间戳、size、截断标志）。
2. **`data/index.sqlite`** —— read API 需要的派生列（status、model、token 数、session 关联、`jsonl_path` + `jsonl_offset`）。可删；秒级从第一层重建。WAL 模式，连接池 8。

当 JSONL 与 SQLite 不一致时，JSONL 为准。

### JSONL trace shape

```json
{
  "id": "01HX7K8MS...",
  "ts_start": "2026-05-27T10:23:45.123Z",
  "ts_end":   "2026-05-27T10:23:46.357Z",
  "client":   "172.17.0.5:54321",
  "method":   "POST",
  "path":     "/v1/messages",
  "upstream": "http://gateway:7860",
  "status":   200,
  "req": {
    "headers": {"x-api-key": "sk-***", "anthropic-version": "2023-06-01"},
    "body":    {"model": "claude-sonnet-4-6", "messages": [...], "stream": true}
  },
  "resp": {
    "headers": {"content-type": "text/event-stream", "x-request-id": "..."},
    "events":  [
      {"event": "message_start",       "data": {"message": {"id": "msg_...", ...}}, "t_delta_ms":  12},
      {"event": "content_block_delta", "data": {"delta": {"text": "Hello"}, ...},   "t_delta_ms": 234},
      {"event": "message_delta",       "data": {"usage": {"output_tokens": 8}},     "t_delta_ms": 511},
      {"event": "message_stop",        "data": {},                                  "t_delta_ms": 514}
    ],
    "stream_done": true
  },
  "disconnected":   false,
  "truncated_req":  false,
  "truncated_resp": false
}
```

上面示例的 header 值是为文档脱敏过的；磁盘上的 JSONL 保留客户端发来的原始字节。见下面 [Security](#security)。

完整字段参考见 [ARCHITECTURE § 3](./ARCHITECTURE.md)。

## Protocol coverage

api-log 字节级原样转发每个 HTTP 请求。finalize 阶段对已知的 LLM API shape 做解析；不认识的 path 仍会落 header + 原始 body，只是跳过结构化字段。

- **OpenAI Chat Completions**（`/v1/chat/completions`）—— 请求的 `messages[]`、响应的 `choices[]`，流式（`data: {...}\n\n` 分块）和非流式都覆盖。
- **OpenAI Responses**（`/v1/responses`）—— 请求的 `input[]`、响应的 `output[]`，包括 `response.output_item.added` 这类 SSE 事件，用于 tool-call 抽取。
- **Anthropic Messages**（`/v1/messages`）—— 请求的 `messages[]` + `system`、响应的 `content[]`，SSE 事件 `message_start` / `content_block_delta` / `message_delta` / `message_stop`。
- **SSE streams** —— 每个 `event:` / `data:` 对切成 `resp.events[]` 里一项，`t_delta_ms` 相对响应起点记录。非 SSE 响应作为解析后的 JSON 落在 `resp.body`。
- **After-mutation capture for streams** —— 当插件改写流式响应（Phase B/C），api-log 记录客户端实际收到的字节，而非上游未改写前的流。

tool call、reasoning block、以及这些协议承载的其他内容原样位于 `req.body` 和 `resp.body` / `resp.events[]` 中。不会被提到顶层字段，api-log 也不做解释。

## Query examples

下面这些是参考片段，不是 benchmark 工作负载。请按自己的数据跑。

### jq over JSONL

跨已录的 Responses-API trace 统计 tool-call 频率：

```bash
zcat data/2026-*/*.jsonl{,.gz} 2>/dev/null \
  | jq -r '.resp.events[]? | select(.event=="response.output_item.added")
           | .data.item | select(.type=="function_call") | .name' \
  | sort | uniq -c | sort -rn
```

被中断的流：

```bash
jq 'select(.disconnected==true or .resp.stream_done==false)
    | {id, path, status, ts_start}' \
  data/2026-05-27/*.jsonl
```

### sqlite3 over the index

按 model 和 path 看最近的失败 trace：

```bash
sqlite3 data/index.sqlite \
  "SELECT ts_start, model, path, status FROM traces
   WHERE status >= 400
   ORDER BY ts_start DESC LIMIT 20"
```

按时间列出某个 session 的所有 trace：

```bash
sqlite3 data/index.sqlite \
  "SELECT id, jsonl_path, jsonl_offset FROM traces
   WHERE session_root_id='01HX7K...' ORDER BY ts_start"
```

`jsonl_path` + `jsonl_offset` 这对值让消费方直接 seek 到 JSONL 文件中的对应行。做批量分析的 AI agent 可以绕过 read API，直接从磁盘读 JSONL。

### Replay a recorded SSE stream

```bash
curl -N -H "Authorization: Bearer $TOKEN" \
  "http://localhost:7862/api/traces/01HX7K8MS.../replay?speed=2"
```

`/api/traces/:id/replay` 按录制时的 per-chunk 间隔重新吐出 SSE 帧（或以 `speed` 倍率加速）。`speed=2` 把间隔减半；`nodelay=1` 把所有事件背靠背一次性吐完。重放只对 API 调用方进行，不会回联上游 LLM。完整语义（包括重解析后缺 `t_delta_ms` 的 trace 如何处理）见 [ARCHITECTURE § 6.4](./ARCHITECTURE.md)。

## Read API

read API 监听独立端口（默认 `:7862`），和 proxy 分开。一共 13 条路由；完整表面见 [ARCHITECTURE § 6](./ARCHITECTURE.md)。要点：

- `GET /healthz` —— 不鉴权；暴露内存里的 drop / overflow 计数器，让运维不用 grep 日志就能发现 capture 退化。
- `GET /api/traces` —— 列表，SQLite 后端，支持 `since` / `until` / `status`（精确值或 `2xx` / `4xx` / `5xx` 桶）/ `model` / `path`（结尾 `*` 表示前缀）/ `key_hash` / `session_root_id` / `project` / `limit` / 游标分页。
- `GET /api/traces/:id` —— 详情；返回 `{row, trace}`，调用方一次往返同时拿到 SQLite 行和解析后的 JSONL 行。
- `GET /api/traces/:id/replay` —— 保留 pacing 的 SSE 重放。
- `GET /api/sessions` —— 按 `session_root_id` 聚合的 session 摘要。只做行级聚合，不做树遍历。
- `GET /api/export` —— 流式吐出匹配 JSONL 行的 zip + 打包的 `agent/CLAUDE.md`，供离线 / AI 辅助分析。
- `GET/PUT /api/config/plugins`（+ `PUT /api/config/plugins/:id`、`DELETE /api/config/plugins`、`GET /api/plugins/types`）—— 热更 `text-replace` / `text-append` / `path-filter` 插件。YAML 仍是声明式的真相；运行时覆盖持久化到 `data/runtime_overrides.json`。默认关闭。

除 `/healthz` 外，所有 read 端点都要求 `Authorization: Bearer <data/admin_token>`，并以 `Cache-Control: no-store` 响应。

api-log **不内嵌 HTML viewer**。`GET /` 返回一个 JSON 指针，指向独立的 [api-log-viewer](https://github.com/xiayangzhang/api-log-viewer) 项目；二进制里没有任何 HTML。

## Security

**Bearer token 在磁盘上不做脱敏。** JSONL 文件里的 `Authorization` / `x-api-key` header 就是客户端原样发来的字节。api-log 在 capture 路径上不做任何脱敏。把 `data/` 目录当成 `~/.ssh/` 或装生产 API key 的文件来对待：

- 把 `data/` 挂到一个有严格文件系统权限的路径（`chmod 700` 或更严）。
- 在没有反向代理做 transport security 与访问控制的前提下，不要把 proxy listener（`:7861`）或 read API listener（`:7862`）暴露到不可信网络。
- `data/admin_token` 处自动生成的 admin bearer 是 read API 唯一的凭据。删文件重启即轮换。
- 同主机容器之间的 plain HTTP 是主要支持的拓扑。如果需要 TLS 或跨主机路由，用你已有的反向代理终结；api-log 自身只听 HTTP。

脱敏是 capture 路径上**有意为之的非目标**。需要脱敏 trace 的话，针对 JSONL 文件跑一个 sidecar；磁盘格式已文档化且稳定。

威胁模型与漏洞披露流程见 [SECURITY.md](./SECURITY.md)。

## Development

```bash
git clone https://github.com/xiayangzhang/api-log.git
cd api-log

# unit + integration tests (race detector on)
go test ./...

# lint (golangci-lint v2)
golangci-lint run

# build
go build -o bin/api-log ./cmd/api-log

# the dev-stack integration harness
./tests/integration/run.sh
```

项目包含 23 个 Go 包，race 检测下测试全过，CI 用 `golangci-lint v2` 跑 lint。每个 push 与 PR 都触发 CI；见 [`.github/workflows/ci.yml`](./.github/workflows/ci.yml)。

## Roadmap

- [x] **v0** —— capture 路径（解析 + JSONL 落盘 + SQLite 镜像）、session 推断、最小 read API。
- [x] **v0.1 viewer** —— [api-log-viewer](https://github.com/xiayangzhang/api-log-viewer) —— 多实例聚合、session 树可视化、tool-call 渲染、SSE 重放。
- [x] **v0.1 plugins** —— `text-replace` / `text-append` / `path-filter`，通过 `PUT /api/config/plugins` 热更。默认关闭。
- [ ] **v0.2** —— 可选的每网关 **bridge adapter**（独立项目）—— 按 `key_hash` 把外部数据（CPA 的 Redis usage queue、new-api 的 MySQL 日志表）join 进 api-log trace。核心 proxy 仍保持网关无关。

完整列表（含 `v0.1.0-deferred` 区段）见 [ROADMAP.md](./ROADMAP.md)。

## Acknowledgements

### Design influence

- **tcpdump / pcap** —— append-only 抓取，解释延后
- **CLIProxyAPI (CPA)** —— 单二进制、单配置的审美
- **Claude Code / Codex CLI** —— 把本地 JSONL session 文件当作可用格式
- **Langfuse** —— LLM 可观测性这一面，本项目在 capture-vs-instrument 这一轴上有意作出不同选择

### Live-traffic iteration partner

- **sub2api** —— 用来在真实流量下验证 capture 路径的主要上游网关；path-filter 模式集合与 client-identification 分类法都是对着它的真实流量样本调出来的。

### Development assistance

本项目代码与文档由 **Claude Opus 4.8**（Anthropic）作为主要 pair-programmer 完成，并由 **GPT-5.5**（通过 Codex CLI）作为 research + review 助手——发布前 adversarial review、对照 OSS 参考项目做 README 结构分析、事实交叉验证。保留 / 删除 / 修订的判断由人类作者负责；这里列出 AI 协助是为了透明，不是 authorship。

## License

[MIT](./LICENSE) —— © 2026 Leo Yun.
