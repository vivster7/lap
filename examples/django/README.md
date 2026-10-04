# dev for Django

An inner-loop command for [django/django](https://github.com/django/django)
(Python, ~7k files, ~20k tests). It mirrors Django's pre-commit hooks and
adds tests:

| task | phase | notes |
|---|---|---|
| `black`, `isort` | prepare (writers) | changed `.py` files; run in order |
| `biome` | prepare (writer) | changed JS/CSS/JSON |
| `flake8` | check | changed files, `-j` = granted CPUs |
| `tests` | check | `runtests.py` full suite (estimate 10m), then **related test apps** |

The "related" variant maps changed paths to `tests/` apps: edited
`tests/<app>/…` directories, and apps named after edited `django/` packages
(`django/contrib/auth/forms.py` → `auth_tests`). It is a heuristic, not impact
analysis, and the report says the full suite was not verified.

## Setup

```sh
cp mise.toml /path/to/django/ && cd /path/to/django
mise install && mise run setup
mise run dev
```

## A real run

Adding an unused `import os,sys` and a badly formatted function to
`django/contrib/auth/forms.py`:

```
dev · budget 1m0s · fix mode · 9 cpu · 1 changed files vs origin/main (merge-base a461af8ce4)
plan: 4 to run, 0 deferred, 1 skipped (dev plan for details)
  ▸ black [default · 1 file · 1 cpu]
  ✎ black 400ms fixed 1 file(s)
  ▸ isort [default · 1 file · 1 cpu]
  ✎ isort 100ms fixed 1 file(s)
  ▸ flake8 [default · 1 file · 5 cpu]
  ▸ tests [related · 4 cpu]
  ✗ flake8 200ms 2 findings
  ✓ tests 29.7s narrowed: broader variant did not fit

── flake8 (findings, exit 1) ──
django/contrib/auth/forms.py:2:1: F401 'os' imported but unused
django/contrib/auth/forms.py:3:1: F401 'sys' imported but unused

dev in 30.4s: 1 passed, 2 fixed, 1 findings
not verified locally: tests (narrowed to related)
```

black and isort ran one after the other (they touch the same file), flake8
started only after both, and `auth_tests` ran instead of the full suite.
(Since that run, per-file tasks get at most one CPU per file.)
