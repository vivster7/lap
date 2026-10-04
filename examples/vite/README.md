# dev for Vite

An inner-loop command for the [vitejs/vite](https://github.com/vitejs/vite)
monorepo (TypeScript, pnpm workspaces):

| task | phase | notes |
|---|---|---|
| `oxfmt` | prepare (writer) | changed files |
| `eslint` | check | changed files, JSON results parsed into findings |
| `build-vite` | check | rebuilds `packages/vite` (~4s) when it changed |
| `typecheck-<package>` | check | one task per package, **selected by path**; `typecheck-vite` runs after `build-vite` |
| `vitest` | check | `vitest run`, then `vitest run --changed <merge-base>`; after `build-vite` |

The e2e suites (playwright) are left to CI.

Vite's unit and type tests import the *built* package, so `build-vite` is an
explicit prerequisite (`After`) — a dependency the generic tool graph cannot
infer, declared in ten lines.

## Setup

```sh
cp mise.toml /path/to/vite/ && cd /path/to/vite
mise install && mise run setup   # pnpm install + pnpm build once
mise run dev
```

## A real run

Adding `export function lapDemo( x:number ):string { return x*2 }` to
`packages/vite/src/node/utils.ts`:

```
dev · budget 1m0s · fix mode · 9 cpu · 1 changed files vs origin/main (merge-base 10033218d2)
plan: 5 to run, 0 deferred, 3 skipped (dev plan for details)
  ▸ oxfmt [default · 1 file · 1 cpu]
  ✎ oxfmt 100ms fixed 1 file(s)
  ▸ eslint [default · 1 file · 1 cpu]
  ▸ build-vite [default · 2 cpu]
  ✓ eslint 1s
  ✓ build-vite 4.3s
  ▸ vitest [full · 5 cpu]
  ▸ typecheck-vite [default · 1 cpu]
  ✓ vitest 3.7s
  ✗ typecheck-vite 8.9s 1 finding

── typecheck-vite (findings, exit 2) ──
src/node/utils.ts:2024:3 - error TS2322: Type 'number' is not assignable to type 'string'.

2024   return x * 2
       ~~~~~~

dev in 13.3s: 4 passed, 1 findings
```

Only `typecheck-vite` was selected (the three other packages' typechecks were
skipped: no matching changes), and the whole unit suite fit in the budget, so
it ran in full.
