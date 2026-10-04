# lap design (v0)

Design produced in AI-assisted review rounds (Claude and Codex), 2026-10-01, before implementation; the "Spike findings" section at the end records what the spikes measured. Partial-run exit policy: **exit 0, show partial coverage** (a stricter opt-in flag may come later).

Build a small framework for composing a company's own local verification command. Its job is to run useful formatting, generation, linting, typechecking, and tests within a configurable time budget while keeping the laptop usable. Scheduling is the central feature.

The proposed deliverable is a library and a company-owned starter program. Mise supplies tool installation, versions, and environments. The framework owns scheduling, process supervision, terminal capture, and a local record of runs. CI integration is an optional use case and does not constrain the architecture.

This draft incorporates the user's direction and review with the existing Claude conversation. Proposed contracts and defaults below still need implementation and acceptance tests. Only the explicitly described local probes are measured evidence.

## Product boundaries

The user's explicit requirements are parallel local work, formatter and generator ordering, CPU awareness, a roughly 60-second configurable budget, mise integration, and an adaptable framework. Timing and outcome telemetry, inspection of previous command output, and correct PTY behavior are core requirements. Storage should be generous: retaining several gigabytes is acceptable. Concurrent runs in multiple worktrees are a v0 design concern. The user prefers a separate prototype, with possible upstreaming later, and removed CI unification as a primary goal.

Changed-file selection, duration history, memory accounting, explainable plans, and repeat-run performance are proposed ways to meet those requirements. They are not reasons to build a second build system.

Keep out of v0: mandatory CI detection or parity, sharding, automatic project dependency inference, remote execution, a persistent background daemon, an independent result cache, a transactional filesystem layer, and a universal configuration language. The user confirmed Mario Zechner's pi as the inspiration and chose Go for the first implementation. Apply the stated preference for a small core and user-owned composition; runtime loading of extensions is not a requirement for v0.

## Core and extension boundaries

Start with modules and clear interfaces; there is no need to publish a separate package for every module.

| Component | Core responsibility | Caller or extension responsibility |
| --- | --- | --- |
| Scheduler | Validate dependencies, coordinate CPU and memory leases across runs, enforce the deadline | Priority, variants, admission policy, resource estimates |
| Process supervisor | Own commands and leases through cancellation and collection | Foreground commands or tested server-specific adapters |
| Terminal capture | PTY and pipe sessions, byte capture, replay and text views | Per-tool capture mode and supported terminal behavior |
| Run store | Run metadata, outcomes, timing history, output artifacts and queries | Retention policy and opt-in metadata export |
| Remediation rules | Match findings, schedule bounded fix attempts and invalidate affected results | Declared fixes, matching predicates, suggest or apply policy |
| Task adapters | Common invocation and result contract | Arguments, worker controls, check/fix behavior and diagnostics |
| Scope helper | Optional Git selection and fingerprint helpers | Roots, base revision, execution inputs and generated outputs |
| Results and events | Preserve execution, coverage, freshness and capture quality | Console, JSON, exit codes and editor integrations |
| Environment resolver | Cache effective environments during one run | Mise integration and explicit dependency preparation |

A Policy proposes admissions. It cannot make the core spend unavailable tokens, violate dependency order, or accept work after the admission cutoff. The core validates every proposal.

Extensions are trusted code, not a security sandbox. Guarantees cover commands launched through the supervisor. An extension that independently spawns processes or blocks the control loop is outside that contract. Reporter callbacks must not block cancellation. Capture streams to disk with bounded memory; generous disk retention does not imply unlimited RAM. Any lost output is explicit, never silent.

## Go extension interfaces

Go is the chosen implementation language. First-class extensions are ordinary Go packages implementing exported interfaces. Each company owns a small main program that imports the framework, registers tasks and adapters, selects a Policy and Reporter, and builds its own dev executable. This is compile-time extensibility; users rebuild after changing Go extensions.

Avoid Go's native plugin package in v0. Its platform limits and strict toolchain/dependency matching add deployment constraints that ordinary Go modules avoid. Rust does not inherently solve this: its native Rust ABI has no stability guarantee either. Both languages can use an explicit IPC or scripting boundary if runtime extensions become a real requirement. [Go plugin documentation](https://pkg.go.dev/plugin), [Rust ABI reference](https://doc.rust-lang.org/reference/items/external-blocks.html#abi)

Later options include external adapters over a versioned JSON protocol or an embedded scripting language. Choose one only when extension authors need it. Neither native dynamic loading nor a scripting runtime is necessary to prove the scheduler.

The following is an interface sketch, not a complete compilable implementation. Supporting value types are intentionally omitted.

```go
type ResourceRequest struct {
    CPUSlots    int
    MemoryBytes *uint64 // nil means unknown
}

type Variant struct {
    ID          string
    Coverage    string
    BreadthRank int // author-defined; no inferred equivalence
    Resources   ResourceRequest
    Estimate    time.Duration // zero means unknown
    Capture     CaptureSpec // PTY, pipes, or stderr PTY + stdout pipe
    Command     CommandSpec
}

type Task struct {
    ID       string
    Phase    Phase // prepare, check, or remediation
    Priority int
    After    []string
    Effect   Effect // workspace write or no source write
    Scope    ScopeSpec
    Outputs  []PathPattern
    Variants []Variant
}

type Policy interface {
    Propose(context.Context, PolicySnapshot) ([]Admission, error)
}
// PolicySnapshot contains ready tasks, estimates, remaining time,
// and available CPU/memory. The core validates returned admissions.

type Adapter interface {
    Materialize(context.Context, TaskRequest) (CommandSpec, error)
    Classify(ProcessResult, CaptureReader) TaskResult
}

type Reporter interface {
    Report(Event)
}
// Reporter delivery must not block process supervision.
// Store persists capture independently of Reporter delivery.

type RunStore interface {
    Last(context.Context, TaskQuery) (TaskRecord, error)
    Runs(context.Context, RunQuery) ([]RunRecord, error)
    OpenCapture(context.Context, CaptureRef) (CaptureReader, error)
}
// Query APIs shown; the write/session lifecycle remains to be designed.

type Rule struct {
    Match  FindingPredicate
    Action Task // a declared writer with inputs and outputs
    Mode   FixMode // suggest or apply
}

type RunResult struct {
    Tasks    []TaskResult
    Findings []Finding
    Coverage CoverageSummary
    Cleanup  CleanupSummary
    Record   RunRef
    Capture  CaptureSummary
    Elapsed  time.Duration
}
```

An adapter materializes a command using the selected scope, tool environment, worker allocation, and capture mode, then classifies its exit status and captured output. Capture references support streaming readers instead of loading a multi-gigabyte log into memory. A Reporter consumes structured events; the run store is the common source for history and later inspection. Empty file selections must not accidentally become “check everything.”

Variants are explicit alternative invocations supplied by the project: for example, a full test suite and an affected-package subset. The framework never invents a sound subset by passing changed filenames to arbitrary tests or typecheckers.

## Starter behavior

The starter is an example people own and edit. It can expose a bare dev command, groups such as lint or test, a plan view, a timeout, check-only and autofix options, and logs, why, and stats queries over the run store. Those names and flags are outside the core API.

Proposed defaults are a 60-second run, conservative resource headroom, automatic local formatting where configured, preparation before validation, a concise live report, and a machine-readable result. Check-only mode verifies without rewriting source; caches and temporary files remain allowed.

Acquire a lock for the current worktree and return busy promptly if another invocation holds it. Separate worktrees may run concurrently through the shared resource pool. Declare additional locks for shared writable outputs or services; separate Git worktrees do not prove all task effects are disjoint. These locks coordinate participating framework runs, not editors or arbitrary external commands.

Resolve the effective mise environment per task directory and tool configuration, then reuse it during the run. Nested project tool versions must not be flattened into a single root environment. Tool and dependency installation are an explicit setup action before the verification invocation. Supplied adapters should avoid implicit downloads or environment sync and report missing prerequisites. Arbitrary project scripts must honor that contract too.

## Scheduling policy

The starter policy is deliberately replaceable. Its first version uses the following procedure:

1. Select applicable tasks, validate the graph, and include their prerequisites. Reject cycles or missing dependencies before execution.

2. Run preparation first. Order writers explicitly; writers that may overlap run sequentially, and each consumes the previous writer's completed output. Independent preparation tasks may run together when their writes are declared disjoint.

3. Once preparation is complete, consider ready checks by declared priority, then shorter conservative estimated duration, with a stable tie-break.

4. For each candidate, propose the broadest declared variant that fits the remaining work window and available CPU/memory. Admit higher-priority ready work first.

5. Reconsider pending work as tasks finish. Record why a variant was narrowed or a task was not admitted.

6. Stop admitting work at the work cutoff and enter cancellation/reporting.

In v0, validators depend on a conservative preparation barrier. If preparation cannot finish, report it as unverified and the validators as blocked. This can leave an otherwise independent cheap check undone; bypassing the barrier through finer dependencies is a later improvement, not hidden inference.

Cost feasibility must account for unfinished prerequisites and resource contention. A leaf that takes two seconds is not a two-second job if it requires a fifty-second producer. Planning predictions are estimates; they do not reserve a guarantee of completion.

Use configured estimates initially, then local history with a margin when there are enough comparable samples. Match variant, tool/configuration, allocated workers, machine, and meaningful scope-size categories. Distinguish sparse history and cold versus warm runs. Exact file hashes belong to freshness checks, not timing keys that would change on every edit. Record resource wait separately from execution time and tag overlapping framework runs. Keep contended samples separate from uncontended estimates; do not silently discard slow runs or treat “no other framework run” as proof the machine was idle.

Unknown work must remain runnable: use an adapter or task estimate when available; otherwise label the estimate unknown, schedule unknown validators after comparable known work, and limit concurrent unknown work. Unknown preparation may consume the remaining work window. The deadline remains the backstop.

Timeout samples mean “at least this long”; do not feed the observed cutoff back as a fast successful duration. Do not claim a p90 from a handful of samples.

This policy is a heuristic. It does not guarantee optimal packing, and longest-first is not a universal answer. Initially measure completed useful work, time to first actionable result, and wasted work at timeout before adding a more sophisticated policy.

## Resource and deadline contracts

A CPU slot is an accounting unit paired with an actual tool worker limit. Four commands that each launch eight workers are not four units of CPU work. Adapters translate allocations into tool flags or environment variables; changing worker counts also changes the relevant duration estimate.

As an initial value to measure, the starter can reserve about half the CPU capacity available to the process, with overrides, while respecting affinity and container limits where available. Apply both the shared pool allowance and any lower per-run cap. Reserve memory for the interactive system and avoid concurrent heavy tasks whose declared requirements exceed either allowance. Unknown-memory work uses a conservative configurable reservation; it is not free.

These are admission controls, not hard OS enforcement or a guarantee that half the machine remains idle. Lower process priority helps responsiveness. Linux cgroups may strengthen containment and limits where available; they are not a prerequisite for using the library. [Linux cgroup documentation](https://www.kernel.org/doc/html/latest/admin-guide/cgroup-v2.html)

Use an absolute monotonic deadline. The starter establishes it before discovery, environment resolution, planning, preparation, and execution. Those phases, cancellation, and reporting all consume the same allowance. A library caller that starts the deadline later must not advertise a whole-command time bound.

Reserve a small configurable cleanup interval inside the total budget. At its start, stop admissions and terminate running work; escalate to forced termination if needed. An estimate fitting the window never guarantees completion. A task can still time out.

The deadline is an engineering target with measured tolerance on a responsive host. Report actual overruns and cleanup failures. Do not promise a real-time bound on pathological kernel I/O or hide an unbounded apply, setup, or cleanup phase after the timer.

## Resource coordination across worktrees

Use one cooperative resource pool per user on a local machine, shared across repositories and worktrees. This avoids each invocation independently assuming it has the whole laptop. It accounts for participating tasks; other users, browsers, IDEs and unrelated builds remain outside it. Headroom and lower process priority still matter.

Prototype daemonless flock token files in a local runtime directory: XDG_RUNTIME_DIR on Linux, with an appropriate private per-user location on macOS. CPU slots and rounded memory reservations share one versioned pool configuration. A 512 MiB memory quantum is a candidate to measure, not a fixed API contract. Per-run configuration may lower its own allowance; it cannot silently resize or create a second incompatible pool.

Acquire the complete CPU and memory request before launch. A short arbitration lock can serialize the nonblocking attempt; on failure, release partial reservations and retry later without holding resources while waiting. Resource waiting consumes the original deadline. V0 need not promise fairness: large requests may starve, and the plan must explain that. Oversized requests are rejected or use an explicitly smaller variant. Keep holder metadata for diagnostics, with process identity rather than a bare reusable PID. Do not unlink or replace active token files.

A flock's lifetime follows its open file description: closing all its descriptors or explicitly unlocking releases it. Therefore the runner's death alone cannot establish that its workload stopped. This design consequence is confirmed by the local lease probe. [Linux flock semantics](https://man7.org/linux/man-pages/man2/flock.2.html)

The initial implementation keeps leases in the runner and releases them after supervised work ends during normal completion, cancellation and timeout. Do not rely on arbitrary tools forwarding inherited lock descriptors. If the runner itself is killed without cleanup, descendants can survive after its locks disappear. V0 resource accounting is therefore best effort after an uncatchable runner failure. A free token is not evidence that no orphaned work exists; the UI and documentation must not promise otherwise.

Record task and owner identity to diagnose unfinished runs. Write a launching state before spawn and complete its identity afterward; recovery treats an unfinished launching record as uncertain. Automatic recovery needs a separate decision after the spike. Lazy cleanup on the next run must solve the launch-before-record crash window, process-group identity after its leader exits, and PID reuse before sending signals; uncertain identity must not authorize killing unrelated processes. A short-lived supervisor could retain leases and cancel work after front-end death, but moves capture and process ownership into another process and still has its own failure boundary. Neither mechanism is required for the first v0 prototype or accepted as proven crash containment.

Shared writable outputs, caches and servers require adapter-specific coordination and accounting. A Bazel server's lifetime and sharing depend on its configuration; neither one server per worktree nor safe concurrent cache writes is a universal assumption.

## Process supervision

Baseline support should cover controlled foreground workloads on Linux and macOS: owned process groups, graceful termination, forced termination, output draining, and reaping direct children. Verify that live work in the owned groups has ended; do not equate the launcher exiting with cleanup.

In Go, creating a process group is only part of this implementation. exec.CommandContext cancels by killing the command's direct Process by default; it does not automatically terminate that group. The supervisor needs explicit group cancellation and verification, with platform-specific code and descendant tests. [Go process cancellation](https://pkg.go.dev/os/exec#CommandContext)

Use different launch setup for capture modes. A pipe child can start in a new process group. A PTY child needs its own session and controlling terminal; creack/pty uses Setsid and Setctty. Do not combine that setup with an unnecessary Setpgid. Track the actual owned groups, terminate them, continue draining within the cleanup allowance, and report an incomplete tail if draining cannot finish. Closing a PTY master is not a substitute for verified cleanup. [creack/pty session setup](https://github.com/creack/pty/blob/master/start.go)

Detached workers and pre-existing shared servers need a tested adapter or an explicit unsupported-containment result. Process traversal cannot close every escape race. Never kill an unrelated Bazel or other shared server to satisfy a local timeout. Prefer standalone Gazelle where it matches the project's configured generator; custom Bazel/Gazelle extensions may require a different adapter.

Mise's task timeout is not sufficient evidence of this property. In the installed Linux mise 2026.9.14, a disposable probe returned at 2.018 seconds for a two-second timeout. At 3.518 seconds a normal Python grandchild was still alive and had written a marker after the deadline; the parent had exited. The test used no daemonization or ignored signals, and test-owned processes were cleaned up. This is an observation of one version and environment, not a claim about all versions or platforms.

The mise probe and its observed result were saved locally by the design reviewers (not in this repository). A second disposable Linux probe, lease_probe.py, killed a runner holding a flock: another process immediately acquired the token while the previous child remained alive. Test processes were cleaned up. This demonstrates the limitation of runner-held locks; it does not validate the proposed supervisor.

## Terminal capture and replay

Make term a reusable core module alongside proc. The default human-readable command uses a PTY; adapters can choose separate pipes for structured output, or a stdout pipe plus a stderr PTY. A single PTY merges stdout and stderr. Separate streams retain identity but only observed read order, not a perfect global chronology.

Keep three views of a capture. The primary record preserves the exact bytes received, with stream IDs, monotonic timestamps and terminal-size events. A rendered view reconstructs terminal state and scrollback with an emulator. A plain searchable view removes styling while preserving useful text history. Rendered and plain views can be derived or cached, with their renderer version recorded; raw capture remains authoritative.

Asciicast is a useful replay export, not the byte-exact primary format: its output events require valid UTF-8 JSON strings. Use byte artifacts plus a chunk journal, or another binary-safe representation, and explicitly handle incomplete records after a crash. Preserve PTY line endings in the primary record and normalize only derived text. [Asciicast v2 format](https://docs.asciinema.org/manual/asciicast/v2/)

A final terminal screen is not a complete diagnostic history. Cursor movement, erasure and alternate-screen use can hide earlier text. Keep replay and a chronological text projection as well as the final screen; document projection limitations. Structured findings are the preferred input for rules. An absence of matches in a lossy projection does not prove a clean check.

Drain each PTY or pipe independently of the display. Stream capture to disk with bounded buffers; display and expensive derived rendering can lag or be rebuilt. They must not block cancellation. Slow or failed storage cannot support unlimited lossless capture: the proposed fallback keeps draining, records missing ranges and capture_incomplete, and disables output-dependent success classification and automatic remediation for the affected task. Caller policy may instead cancel it.

Treat process exit and output completion as separate events. Test Linux PTY hangup/EIO handling and macOS behavior, normal exit tails, descendants holding the slave open, and forced cancellation. Only expected hangup conditions become EOF; arbitrary I/O errors must remain visible. Finish draining within the cleanup allowance, then report any missing tail.

Record initial terminal dimensions, resize events, TERM and relevant color policy. Use a tested default size when there is no user terminal. Advertised terminal capabilities must match the emulator; respect NO_COLOR. Test query/reply sequences as well as output rendering. No human stdin is routed to parallel jobs by default. Disabling echo is not a noninteractive guarantee: adapters must suppress prompts through supported flags or environment settings, and a quiet process is not enough evidence to diagnose a prompt.

For live reporting, show per-task status and separately rendered output. Mixing cursor-control streams from several tasks into the user's terminal is invalid. Candidate dependencies to evaluate in the spike are creack/pty and Charm's experimental vt emulator; pin and test them before committing to their behavior. [creack/pty](https://github.com/creack/pty), [Charm terminal packages](https://github.com/charmbracelet/x/blob/main/README.md)

## Run storage and telemetry

Store timing, outcomes and output references in one run record. The scheduler and inspection commands query that record rather than maintaining inconsistent histories. Use versioned metadata, unique run and task-attempt IDs, and a durable started/completed distinction; an interrupted run remains inspectable.

A Git-backed default layout can share the history index under the common Git directory while keeping captures in each worktree's Git directory. Resolve these locations through Git, since .git may be a file. Non-Git callers supply their own store root. [Git directory discovery](https://git-scm.com/docs/git-rev-parse)

```text
<git-common-dir>/devloop/history.jsonl
<git-dir>/devloop/runs/<run-id>/run.json
<git-dir>/devloop/runs/<run-id>/tasks/<attempt-id>/meta.json
<git-dir>/devloop/runs/<run-id>/tasks/<attempt-id>/output.bytes
<git-dir>/devloop/runs/<run-id>/tasks/<attempt-id>/events.jsonl
```

Serialize history appends under a short lock and tolerate an incomplete trailing record after a crash. Do not rely on O_APPEND alone to turn a multi-write record into a transaction. Keep per-run records sufficient to rebuild the index. Store failures remain visible; recording/reporting belongs within the run's deadline and cannot hide an unlimited fsync or export.

Per attempt, record task and tool/config identity, variant, scope, workers and reservations, queued/start/end times, wall and CPU time where available, capture mode and references, terminal dimensions, contention, outcome dimensions, and whether elapsed time is censored. Per run, record budget, base and commit, worktree identity, machine capacity, optional battery state, plan and actual deferrals. Missing optional metrics are unknown, not zero. Do not collect the full environment by default.

Label metric source, units and coverage. Linux ru_maxrss for children describes the largest child, not simultaneous memory usage of the whole process tree; CPU accounting also depends on which descendants were waited for. A process high-water mark, sampled tree estimate and cgroup measurement must remain distinct. Start with truthful measurements and declared memory estimates. [Linux resource accounting](https://man7.org/linux/man-pages/man2/getrusage.2.html)

Expose timing distributions and separate rates for findings, execution failures, timeouts, deferrals, completed coverage and stale results. State denominators: deferred work was not attempted, and a narrow variant is not full coverage. Keep remediation attempts distinguishable from first checks. Comparable-history filters include task/config/tool version, variant, scope category, workers, machine and observed contention.

Everything is local by default. An opt-in Reporter can export explicitly selected telemetry metadata, such as OpenTelemetry events; raw logs are excluded from that export. Store queries accept worktree, run, task and status filters. A “last eslint” query must make its worktree and attempt explicit.

Retention should be generous and configurable by age and several gigabytes, with a free-space floor rather than an aggressive few-megabyte per-command cap. Check available space and warn early. A precheck cannot prevent ENOSPC later; capture degradation must be explicit. Coordinate retention across runs, protect active writers and readers, and mark expired artifacts as unavailable. Compress closed older captures outside the verification path, or charge that work to the same deadline. Do not assume a compression ratio.

## Writers and freshness

V0 accepts in-place formatters and generators because they are central to the intended workflow. Preparation is ordered and the deadline still applies to it.

If a writer is interrupted, emit interrupted_writer, report known changed and potentially affected paths, and block dependent validators. Arbitrary in-place tools may leave partial output. The framework does not automatically roll back, promise whole-file atomicity for those tools, or claim the repository is ready to push.

Before/after hashes reveal differences; they cannot identify whether a writer or a user produced them. Checkers fingerprint the relevant inputs after preparation and compare again at completion. Detected changes make affected evidence stale. This is best-effort freshness checking, not snapshot isolation.

Staged output can be a later adapter capability. Some tools already support useful mechanisms: ESLint can return fixes without saving them, and Gazelle can emit a diff without writing BUILD files. [ESLint dry-run fixes](https://eslint.org/docs/latest/use/command-line-interface#--fix-dry-run), [Gazelle output modes](https://github.com/bazel-contrib/bazel-gazelle/blob/master/gazelle-reference.md)

Even then, hash-then-rename is not compare-and-swap, and multiple file replacements are not one transaction. Do not reintroduce those claims through an optional adapter.

## Remediation rules

Rules map structured findings to declared actions; regex over a documented text projection is a fallback. Actions come from project configuration, never from executing instructions found in tool output. The starter suggests diagnostic-triggered fixes by default; an autofix option enables application. This is separate from configured formatters that already run during normal preparation.

V0 permits one remediation pass after normal preparation and checks have finished. Let in-flight checks complete before entering the writer barrier. Matches must be from the current run and worktree, with relevant inputs still current. Old captures can suggest a fix; applying it requires current-worktree validation. Incomplete diagnostic capture cannot trigger automatic remediation.

Before starting, estimate the fix together with the necessary revalidation and cleanup. If it does not fit, retain the suggestion. Apply admitted fixes under the worktree/shared-output locks, resource pool and original deadline. Unknown write scope invalidates conservatively; scope declarations, dependencies and fingerprints identify every potentially affected result, not just the original failing check.

Re-admit affected checks through the normal scheduler. Unaffected current evidence remains useful. Allow at most one remediation pass for the whole run, not one pass per matching rule; conflicting actions need explicit ordering or remain suggestions. A timed-out writer follows the interrupted-writer contract. If revalidation cannot complete, say “fix applied; verification incomplete,” preserving original findings and every attempt in the store. Never call the problem fixed merely because the fix command returned zero.

Installation and setup actions such as dependency sync are suggestions for an explicit setup invocation, outside the timed verification workflow.

## Git selection and actual execution scope

The optional Git helper compares with the configured target/default branch's merge-base and includes committed branch changes, staged and unstaged changes, and eligible untracked files. It reports the base it used and does not fetch automatically. A tracking upstream often points to the same feature branch, so it is not a reliable default target branch. If no base can be resolved, require an explicit base or full scope.

Keep deleted and renamed paths as selection triggers even when they cannot be passed to a file-based tool. Config or lockfile changes can require a wider check. Empty file lists skip file-based invocations; a separately declared project-wide operation may still be applicable.

Selection and execution scope are different concepts. A changed TypeScript file may select a typecheck that must read the whole project. Let tasks declare file versus project execution and relevant config/inputs. Do not silently narrow semantic checks.

After generators run, include their declared outputs and refresh relevant fingerprints before validation. Broad or unknown writes force conservative ordering. Automatic graph discovery and undeclared-output inference are out of scope for v0.

## Plans and outcomes

A plan explains proposed admissions, prerequisites, worker and memory allocations, estimate confidence, chosen scope/variant, and reasons for deferral. It is a prediction, not a promise. The final report records what actually happened.

For illustration only, a large project might complete linting, a project typecheck, and an affected-test variant while leaving the full suite unverified. A small project may run the full suite inside the same budget. These are the product scenarios the prototype must demonstrate, not benchmark claims.

Preserve four independent facts: findings, execution state, coverage, and freshness. Distinguish a real diagnostic from tool failure; deferred work from attempted timeouts; and blocked descendants from tasks that were eligible but did not fit. Track capture completeness and cleanup separately too: a clean exit cannot turn missing diagnostics into verified success.

“Deferred” means unverified locally. It does not assert that CI will run it. A company may attach an external-coverage hint, but that is caller-provided information.

Exit-code mapping belongs to the starter, not the library. Decided (user, 2026-10-01): the starter exits 0 when no findings were produced, even if coverage was partial — a bounded run is expected to not finish everything, and partial coverage is shown in the report and JSON rather than failing the command. 1 for findings, 3 for configuration/execution/cleanup failure, 130 for interruption. A stricter partial-coverage exit code may be added later as an opt-in flag. Mixed outcomes remain fully represented in JSON.

## Prototype scope and acceptance

Start with a joint process-supervision and terminal-capture spike, including cancellation, output retention and lease lifetime. Then build scheduling, the per-user resource pool, task/variant/result interfaces, the shared run store, remediation rules, a simple Git helper, console and JSON reporting, mise integration, and a company-owned starter. Ruff, one JavaScript formatter/linter, standalone Gazelle where applicable, and configurable typecheck/test commands are enough to exercise the model.

Use existing tool caches. Start with versioned local run records and artifacts; a database and a rich terminal UI are optional later choices. PTY capture and a rendered inspection view belong in the first spike. Keep check-only mode and a worktree lock in the starter.

Build the company-owned Go executable during explicit setup or development, then invoke that executable for normal timed runs. A go run launcher may be convenient while iterating, but cold compilation and module downloads cannot be treated as invisible or assumed to fit a tight startup budget. No startup benchmark is established by this design.

| Scenario | Required observation |
| --- | --- |
| Small project | All applicable configured checks, including full tests when feasible, complete within the budget |
| Large project | A declared smaller variant can run; full coverage is visibly unverified |
| Cold start | No invented confident percentile; unknown work has explicit fallback behavior |
| Heavy tasks | CPU reservations and configured memory admissions never exceed the run's allowances |
| Nested parallelism | Tool workers receive the allocation; command count alone is not the limit |
| Expensive preparation | Preparation is unverified, consumers blocked, no silent deadline exemption |
| Time consumed by discovery | Discovery and environment resolution reduce execution time |
| Timeout history | Interrupted durations are recorded as lower bounds |
| Normal or TERM-resistant grandchild | No live owned workload remains after the documented cancellation tolerance |
| Writer interruption | Partial preparation is reported; no rollback and no successful dependent result |
| Deletion or empty file selection | No accidental whole-repository invocation |
| Nested tool configuration | Each task receives the correct mise environment |
| Same worktree or detected edit | Busy returns promptly; detected input changes invalidate evidence |
| Several worktrees | Shared CPU and memory reservations stay within the configured pool; contention is explained |
| Runner killed during work | Reproduce the orphan/token gap; record incomplete work and never claim crash-safe resource accounting from flock alone |
| PTY output | Colors, carriage returns, cursor movement, Unicode split across reads, terminal queries and resize events replay correctly |
| Output at exit and timeout | Tail is drained when possible; partial capture and cleanup limits are explicit on Linux and macOS |
| Slow display or full disk | Cancellation remains responsive; capture loss is marked and incomplete diagnostics cannot trigger an autofix |
| Large logs and concurrent retention | Multi-gigabyte artifacts use bounded RAM; active captures and readers are protected |
| Autofix changes shared inputs | Every affected result is invalidated; one bounded remediation pass, with remaining gaps visible |
| Historical error match | Produces a suggestion; applying requires current-worktree validation |
| Interrupted store update | Records remain recoverable; incomplete runs are never indexed as completed |

Measure representative repositories on an otherwise usable laptop, with both cold and warm tool caches. Report time to first finding, completed coverage, CPU/RAM observations, deadline overruns, and work discarded at timeout. Synthetic cases establish mechanics; real repositories decide whether the scheduler earns its complexity. Include several simultaneous worktrees and slow log consumers. Also measure startup/capture overhead, resource-wait time, and retention cost.

## Prior art and decisions still open

Hk remains valuable prior art for mise integration, selected-file coordination, and fix/check workflows. Its documentation explicitly distinguishes file locks from dependency ordering and warns that tools may create their own workers. Borrow those lessons without nesting an opaque hk scheduler inside this one. [Hk mise integration](https://hk.jdx.dev/mise_integration), [Hk dependencies](https://hk.jdx.dev/configuration#dependencies-and-groups), [Hk job limits](https://hk.jdx.dev/environment_variables.html#hk-jobs)

Mise already provides generic task dependencies and a global task-run timeout. The proposed framework must earn its existence through adaptable scheduling, resource coordination, explainability, and reliable process ownership. [Mise task configuration](https://mise.jdx.dev/tasks/task-configuration.html#timeout)

The core language is Go and the pi reference is confirmed. The starter's partial-success exit policy is decided: exit 0 with visible partial coverage. Whether companies later need runtime-loaded or non-Go extensions should be established through actual extension authors.

Windows/ConPTY, automatic orphan recovery or a separate supervisor process, fairness between runs, optional staged writers, finer preparation dependencies, and result caching should follow evidence from the prototype. The shared resource pool itself is in v0.

The next implementation milestone is to test the proposed contracts together: PTY behavior, process and lease lifetime, durable output, and cancellation within the budget. Passing a synthetic probe is not evidence that arbitrary tools or detached servers satisfy those contracts.

## Spike findings (2026-10-01)

Measured on Linux (Go 1.27.1); darwin compiles but is unrun. Details and numbers: [proc/SPIKE.md](../proc/SPIKE.md), [term/SPIKE.md](../term/SPIKE.md). These supersede conflicting statements above.

Process supervision (`proc`):
- `Setsid` + `Setpgid` together is invalid: start fails with EPERM. `Setsid` alone gives pgid == sid == pid. PTY mode owns the *session*; pipe mode owns the *process group*.
- `exec.Cmd` is not used: default cancellation and the `WaitDelay` fallback both kill only the direct child, it cannot observe exit without reaping, and it resolves argv[0] against the supervisor's PATH. `proc` uses `syscall.ForkExec` + pidfd.
- Observe exit without reaping; reap only after cleanup is verified. The zombie pins the pid so group signals can never hit a recycled ID; single-pid signals re-check start time via pidfd.
- A grandchild that calls `setsid` escapes group and session scans. It is found while its parent lives (ancestry) or while it holds the task's stdio (fd scan, which requires per-task output fds from `term`). Orphaned with stdio closed, it is undetectable — a known gap; child-subreaper is an option, not built.
- PTY sessions receive kernel SIGHUP when the session leader exits or the master closes. TERM-resistance tests in PTY mode must also ignore HUP. The "runner killed" acceptance case must be tested per capture mode.
- Leases: release by `close()` after verified cleanup, never `LOCK_UN`; token fds are close-on-exec; capacity is never reported free when verification failed.
- `Pdeathsig` reaches only the direct child and fires when the starting OS thread exits.
- `ru_maxrss` is KiB on Linux, bytes on darwin.
- Cleanup latency: TERM→verified ≈12 ms, KILL→verified ≈7 ms.

Terminal capture (`term`):
- creack/pty v1.1.24 returns a *blocking* master (its `Fd()` calls switch the fd to blocking), so read deadlines and Close cannot unblock reads. `term` re-wraps the master non-blocking on Linux; macOS remains blocking (open issue).
- Linux EIO after child exit is treated as EOF only for PTYs; 2000 write-then-exit runs lost 0 bytes. EIO arrives only after *every* slave copy is closed, including the parent's — closing child ends after start is part of the launch contract.
- Leftover descendants: in PTY mode an ordinary `cmd &` is hung up when the child exits; only HUP-ignoring processes keep output open. In pipe mode any background job keeps it open. Launcher sequence: child exits → short tail grace on `Drained()` → terminate group/session → `Done(cleanup ctx)` (implemented in `run`).
- Terminal queries: a child awaiting a cursor-position reply hangs forever without one. `term` answers from a live emulator by default (bounded queue, 1 s timeout), journaling replies as `input` events. Line discipline keeps cooked kernel defaults; raw bytes (with CRLF) are stored as received.
- Emulator: `github.com/charmbracelet/x/vt` (pinned pseudo-version), chosen over vt10x and go-headless-term for wide/emoji correctness and query replies. Its gaps — split combining marks, an unbuffered reply pipe, in-memory scrollback erased by `clear`, ~3–6 MB/s rendering — are worked around in `term`. The rendered view is therefore lazy and tail-limited; the plain projection is the scalable view.
- Throughput with bounded RSS (19–32 MB at any output size): PTY 156–183 MB/s, pipes 1.2–2 GB/s. A slow display does not slow draining. A disk failing after 100 KB still drains, records the exact missing range, and marks the capture incomplete.
- Each capture directory holds `output.bytes`, `events.jsonl`, `capture.json`, and cached derived views.
