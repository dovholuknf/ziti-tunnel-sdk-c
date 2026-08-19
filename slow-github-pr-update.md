# Making the PR workflow cheap to iterate on

## The problem

`Main Test Workflow` takes about 15 minutes on every PR. Measured on run 32271781945:

| phase | window | duration |
|---|---|---|
| builds, 9 jobs in parallel | 0:00 - 6:23 | 6m23s, gated by `linux-arm` |
| integration + compose, 10 jobs in parallel | 6:23 - 13:30 | 7m07s, gated by `main-ha` Windows |

Two serial phases, each as slow as its slowest member. Everything else is free parallelism, so the only
things that matter are the two critical-path jobs and what gates what.

Iterating on the workflow itself is what hurts: every structural change costs a 15-minute round trip,
and a mistake in a workflow file costs a whole run for nothing. That happened - a job-level `if` reading
`matrix.tested` is invalid (only `github`, `inputs`, `needs`, `vars` are available there), GitHub
rejected the file, and the run produced zero jobs.

## What was built

### 1. Local simulator, `scripts/ci-sim`

Reads the workflow files for structure and a measured run for durations, then schedules the jobs and
prints the elapsed estimate, the critical path, and the phases. Sub-second, no pushes.

```
go run ./scripts/ci-sim
```

On the current workflow it estimates 16:05 against an actual 15:45, with the same critical path
(`linux-arm` then `main-ha` Windows). Close enough to compare structures: change a `needs`, re-run, see
where the critical path lands.

Refresh the durations from any run of the same workflow:

```
gh api -X GET repos/openziti/ziti-tunnel-sdk-c/actions/runs/<id>/jobs --paginate \
  --jq '[.jobs[] | {name: .name, seconds: ((.completed_at | fromdate) - (.started_at | fromdate))}]' \
  | tee scripts/ci-sim/timings.json
```

### 2. Stub mode, `CI_STUB=1`

The simulator models ordering; it cannot tell you whether GitHub accepts the file or whether artifact
names line up. Stub mode does that. With `CI_STUB: "1"` set at workflow level in the caller:

- `cmake.yml` skips the compile and fabricates the bundle artifact the downstream jobs download
- `integration-tests.yml` prints what it would have run instead of standing up an overlay
- the compose job prints instead of running docker

The real workflow files run, so the job graph, matrix expansion, gating, artifact names and workflow
syntax are all exercised. A run costs about a minute rather than fifteen. Hosted jobs pay roughly 10-20
seconds each in scheduling and checkout, and the graph has two gated phases, so a stub run cannot get
below about 45 seconds no matter how empty the steps are.

`CI_STUB` must never be set on `main`. It exists to be set on a throwaway branch of a fork.

## Why the fork

Pushing workflow experiments to `openziti/ziti-tunnel-sdk-c` notifies the whole team on every run. The
same branches on `dovholuknf/ziti-tunnel-sdk-c` run the same workflows and notify nobody.

## The optimizations worth making

Measured, in order of payoff:

1. **Stop gating the tests on all nine builds.** The integration jobs download exactly three artifacts:
   `linux-x64`, `macOS-arm64`, `windows-x64-mingw`. They currently wait for `linux-arm`,
   `linux-arm64`, `windows-arm64`, `windows-x64-win32crypto` and `macOS-x64` as well - about 50 seconds
   of pure waiting, more whenever an ARM build is slow. The first attempt at this used a job-level `if`
   on the matrix and was invalid; the working shape is to build the matrix in a `resolve-presets` job
   and feed it through `fromJSON`, since selection has to happen before the matrix exists.

2. **Reconsider whether `main-ha` gates the PR.** It is the slowest test job, the flakiest, and it tests
   ziti `main`, which is unreleased. Moving it to the nightly - where `main` is already covered - takes
   the test phase from about 7 minutes to about 4m30s and removes a recurring PR failure that is not
   about the PR.

3. **Reconsider three ziti channels per PR.** Nine integration jobs cover `latest`, `lts` and
   `lts-maintenance` across three platforms. One channel on PRs with the full matrix nightly is a large
   cut if the extra channels rarely catch anything alone.

1 is mechanical. 2 and 3 trade coverage for latency and are a judgement call.

## What is not the problem

- **The Go module cache.** `setup-go` reports `Cache hit … Cache restored successfully`. The
  `go.sum not found` warning comes from `openziti/ziti/setup-cli`, which runs its own `setup-go` and
  looks for a `go.sum` in this repo's root. It only affects jobs that build ziti from source.
- **`vcpkg`.** The binary cache is keyed and restoring; the Linux builds are slow because they compile
  in the builder container, not because they refetch.
