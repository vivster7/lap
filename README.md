# lap

A small Go framework for building your own bounded inner-loop verification command:
run formatters, generators, linters, typechecks and fast tests in parallel within a
time budget (e.g. 60s), without making the laptop unusable, and report clearly what
didn't get verified.

Inspired by [pi](https://github.com/badlogic/pi-mono): a small core that does the hard
parts well (process supervision, PTY capture, scheduling, run history), and a
company-owned `main` program that composes it. Tools are installed by
[mise](https://mise.jdx.dev).

Status: pre-v0 spikes. See [docs/design.md](docs/design.md).

## Layout

| package | role |
|---|---|
| `proc`  | process supervision: owned groups/sessions, cancellation, cleanup verification |
| `term`  | output capture: PTY/pipe sessions, raw byte journal, rendered + plain views |
| `sched` | (planned) admission, deadline, cross-worktree resource pool |
| `store` | (planned) run records, timing history, capture artifacts |
| `rules` | (planned) finding → remediation |

## Dev

    mise install
    go test ./...
