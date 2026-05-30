# jq Cheatsheet: API Log Analysis

Use these copy-paste jq commands to extract insights from the export. All commands assume you're in the directory containing `data/`.

---

## Error Handling

### Find all HTTP errors (status >= 500)

```bash
jq 'select(.status >= 500)' data/**/*.jsonl
```

### Count errors by status code

```bash
jq -s 'group_by(.status) | map({status: .[0].status, count: length}) | sort_by(-.count)' data/**/*.jsonl
```

### Get error details (status, path, timestamp, message)

```bash
jq 'select(.status >= 400) | {status, path, ts_start, error_msg: (.resp.body.error // .resp.body.message // "unknown")}' data/**/*.jsonl
```

---

## Endpoint Analytics

### Count requests per endpoint (path)

```bash
jq -s 'group_by(.path) | map({path: .[0].path, count: length}) | sort_by(-.count)' data/**/*.jsonl
```

### Count requests per model

```bash
jq -s 'group_by(.req.body.model) | map({model: .[0].req.body.model, count: length}) | sort_by(-.count)' data/**/*.jsonl
```

### Get endpoint response time distribution

```bash
jq '[.ts_start, .ts_end] | ((.[1] | fromdateiso8601) - (.[0] | fromdateiso8601)) as $duration | {path, status, duration_ms: ($duration * 1000 | round)}' data/**/*.jsonl | jq -s 'group_by(.path) | map({path: .[0].path, avg_ms: (map(.duration_ms) | add / length | round), max_ms: (map(.duration_ms) | max), min_ms: (map(.duration_ms) | min)})' 
```

---

## Conversation Reconstruction

### Get all messages in one conversation (session_root_id)

```bash
jq 'select(.session_root_id == "YOUR_SESSION_ROOT_ID") | {id, parent_id, path, method, ts_start, req_body: .req.body, resp_body: .resp.body}' data/**/*.jsonl
```

### Extract chat messages from a multi-turn conversation

For requests with `req.body.messages` (chat API format):

```bash
jq 'select(.session_root_id == "YOUR_SESSION_ROOT_ID" and .path == "/v1/chat/completions") | {ts: .ts_start, role: .req.body.messages[-1].role, content: .req.body.messages[-1].content, model: .req.body.model}' data/**/*.jsonl
```

### Walk a parent_id chain manually

Find the root (parent_id: null), then follow parent_id → id:

```bash
jq 'select(.parent_id == null and .session_root_id == "YOUR_SESSION_ROOT_ID")' data/**/*.jsonl
# Then find the next one: select(.id == "PARENT_ID_FROM_ABOVE")
# Repeat until parent_id is null
```

Or use this one-liner (requires recursive descent):

```bash
jq 'select(.session_root_id == "YOUR_SESSION_ROOT_ID") | {id, parent_id, ts_start, status}' data/**/*.jsonl | jq -s 'sort_by(.ts_start)'
```

---

## Token & Cost Analysis

### Sum completion tokens per model

For OpenAI-style responses with `usage` field:

```bash
jq 'select(.resp.body.usage) | {model: .req.body.model, completion_tokens: .resp.body.usage.completion_tokens, prompt_tokens: .resp.body.usage.prompt_tokens}' data/**/*.jsonl | jq -s 'group_by(.model) | map({model: .[0].model, total_completion: (map(.completion_tokens) | add), total_prompt: (map(.prompt_tokens) | add)})'
```

### Estimate cost (example: GPT-4 pricing)

Assumes pricing: prompt $0.03/1K, completion $0.06/1K

```bash
jq 'select(.resp.body.usage) | {tokens: .resp.body.usage, cost_usd: ((.resp.body.usage.prompt_tokens * 0.03 + .resp.body.usage.completion_tokens * 0.06) / 1000)}' data/**/*.jsonl | jq -s 'map(.cost_usd) | add'
```

---

## Performance Analysis

### Find slowest 10 requests

```bash
jq -s 'map({id, path, status, duration_ms: (((.ts_end | fromdateiso8601) - (.ts_start | fromdateiso8601)) * 1000 | round)}) | sort_by(-.duration_ms) | .[0:10]' data/**/*.jsonl
```

### Requests slower than 5 seconds

```bash
jq 'select(((.ts_end | fromdateiso8601) - (.ts_start | fromdateiso8601)) > 5) | {path, status, duration_sec: (((.ts_end | fromdateiso8601) - (.ts_start | fromdateiso8601)) | round)}' data/**/*.jsonl
```

### P95 / P99 latency per endpoint

```bash
jq -s 'group_by(.path) | map({path: .[0].path, p95: (map((((.ts_end | fromdateiso8601) - (.ts_start | fromdateiso8601)) * 1000)) | sort | .[length * 0.95 | floor]), p99: (map((((.ts_end | fromdateiso8601) - (.ts_start | fromdateiso8601)) * 1000)) | sort | .[length * 0.99 | floor])})' data/**/*.jsonl
```

---

## Session & Key Analysis

### Count unique sessions (session_root_id)

```bash
jq -s 'map(.session_root_id) | unique | length' data/**/*.jsonl
```

### Count unique API keys (key_hash)

```bash
jq -s 'map(.key_hash) | unique | length' data/**/*.jsonl
```

### Show all requests for a specific API key

```bash
jq 'select(.key_hash == "sha256:YOUR_KEY_HASH")' data/**/*.jsonl
```

### Requests per API key (top 10)

```bash
jq -s 'group_by(.key_hash) | map({key_hash: .[0].key_hash, count: length}) | sort_by(-.count) | .[0:10]' data/**/*.jsonl
```

---

## Data Extraction

### Extract all user input messages (chat)

```bash
jq 'select(.req.body.messages) | .req.body.messages[] | select(.role == "user") | .content' data/**/*.jsonl
```

### Extract LLM responses (chat)

```bash
jq 'select(.resp.body.choices) | .resp.body.choices[0].message.content' data/**/*.jsonl
```

### Extract images from generation responses

Assumes image generation API (returns base64 or URL in `data`):

```bash
jq 'select(.path | contains("/images/generations")) | {id, ts: .ts_start, b64_images: .resp.body.data[].b64_json}' data/**/*.jsonl
```

### Extract embeddings (dimension, model)

For embedding requests:

```bash
jq 'select(.path | contains("/embeddings")) | {model: .req.body.model, dimension: (.resp.body.data[0].embedding | length), count: (.resp.body.data | length)}' data/**/*.jsonl
```

---

## Debugging

### Pretty-print a single line

If you found a trace ID you want to inspect:

```bash
jq 'select(.id == "YOUR_TRACE_ID")' data/**/*.jsonl | jq .
```

### Show request headers for debugging auth

```bash
jq 'select(.status == 401) | {id, path, headers: .req.headers}' data/**/*.jsonl
```

### Check for missing or null fields

```bash
jq 'select(.resp.body == null or .req.body == null)' data/**/*.jsonl | head -5
```

---

## Tips

1. **Piping jq to jq**: Combine queries with `|` for readability
   ```bash
   jq '[select(.status >= 500)] | length' data/**/*.jsonl
   ```

2. **Array aggregation (`-s`)**: Collect all results into an array before grouping
   ```bash
   jq -s 'group_by(.path) | ...' data/**/*.jsonl
   ```

3. **Date arithmetic**: Use `fromdateiso8601` and `todateiso8601` for timestamp operations
   ```bash
   jq '.ts_start | fromdateiso8601'
   ```

4. **Slicing and sorting**: `.[]` iterates; `sort_by()` sorts; `.[n:m]` slices
   ```bash
   jq -s 'sort_by(.ts_start) | .[0:10]' data/**/*.jsonl
   ```

5. **Conditional filtering**: Use `select()` to filter based on predicates
   ```bash
   jq 'select(.status >= 500 and .path | contains("/chat"))'
   ```

6. **Testing queries**: Use `head -1 data/**/*.jsonl | jq '...'` to test on a single line first

