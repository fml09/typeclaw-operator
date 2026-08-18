# Domain Docs

How engineering skills should consume this repository's domain documentation
when exploring the codebase.

## Before exploring, read these

- **`CONTEXT.md`** at the repository root, or
- **`CONTEXT-MAP.md`** at the repository root if it exists. It points at one
  `CONTEXT.md` per context; read each one relevant to the topic.
- **`docs/adr/`** for ADRs that touch the area about to change. In
  multi-context repositories, also check `src/<context>/docs/adr/` for
  context-scoped decisions.

If these files do not exist, proceed silently. The `domain-modeling`
technique creates them lazily when terms or decisions are resolved.

## File structure

Single-context repository:

```text
/
├── CONTEXT.md
├── docs/adr/
└── src/
```

Multi-context repository, indicated by `CONTEXT-MAP.md` at the root:

```text
/
├── CONTEXT-MAP.md
├── docs/adr/
└── src/
    ├── ordering/
    │   ├── CONTEXT.md
    │   └── docs/adr/
    └── billing/
        ├── CONTEXT.md
        └── docs/adr/
```

## Use the glossary's vocabulary

When output names a domain concept, use the term defined in `CONTEXT.md`.
Avoid synonyms that the glossary explicitly rejects.

An undefined concept is a signal to reconsider the language or record a real
glossary gap for `domain-modeling`.

## Flag ADR conflicts

Surface conflicts with an existing ADR explicitly instead of silently
overriding the decision.
