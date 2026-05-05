# Architecture Decision Records

This directory holds ADRs in the [Nygard format](https://cognitect.com/blog/2011/11/15/documenting-architecture-decisions).

## File naming

`NNNN-<kebab-title>.md` where `NNNN` is a 4-digit zero-padded sequential id.

## Required header

```text
Status: <Proposed | Accepted | Superseded by NNNN | Deprecated>
Date: YYYY-MM-DD
Deciders: <name>
```

## Sections

`Context` — the forces in play. `Decision` — the choice (one paragraph plus a one-line summary). `Considered alternatives` — at least three, each with concrete pros/cons. `Consequences` — what downstream work inherits.

## Lifecycle

`Proposed` → `Accepted`. Once accepted, an ADR is immutable except for status transitions to `Superseded by NNNN` (link the replacement) or `Deprecated`.

## Index

- [0001-worker-substrate.md](./0001-worker-substrate.md) — worker substrate for the I/O-capable tool path.
