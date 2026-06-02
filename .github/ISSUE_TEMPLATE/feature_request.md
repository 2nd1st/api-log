---
name: Feature request
about: Propose a change to the core proxy, read API, or shipped plugins.
title: ""
labels: enhancement
assignees: ""
---

<!--
api-log is deliberately small. The "What we will not do" section of
ROADMAP.md lists the categories of work that will be closed on sight:
no gateway features (auth / routing / retries / rate limiting / caching
/ rewriting), no SDK instrumentation, no semantic interpretation of
recorded content, no automatic redaction in the capture path, no
matching on credential-carrying header values.

The default answer for new behavior is "off-by-default plugin", not
"core change". If you have not read ROADMAP.md, do that first.

Requests phrased as "wouldn't it be nice if api-log could..." will be
closed. Requests grounded in an adopter use case the maintainer can
reason about will be considered.
-->

## Adopter use case

Concrete situation in your own deployment that this would address.
Which gateway, what kind of traffic, what you are trying to learn or
do that you cannot do today. Numbers help (trace volume, request
shape, time horizon).

## Why this cannot be a plugin

api-log ships a plugin surface (`text-replace`, `text-append`,
`path-filter`, `capture-filter`) and the surface is intentionally
extensible. Before proposing a core change, explain why an
off-by-default plugin — yours, in a fork or sidecar repo — cannot
deliver the same outcome.

## Existing surface that comes close

What in api-log today (read API endpoint, JSONL field, plugin, env
knob) gets partway to your use case? What is the gap?

Link the relevant section of ARCHITECTURE.md or README.md.

## Acceptable scope

To bound review, name what is **in** scope and what is explicitly
**out** of scope for the proposed change.

- **In scope:** ...
- **Out of scope:** ...

A request that does not bound itself tends to grow during
discussion. Bound it up front.

## Alternatives considered

Other ways you considered solving this (downstream tooling over the
JSONL, a sidecar over `data/`, a change to the upstream gateway
instead, etc.) and why the proposal here is the one you landed on.
