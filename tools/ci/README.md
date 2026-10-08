# TTC's CI

`.gitlab-ci.yml` runs TTC's CI on GitLab. `ci.sh` holds its steps, which run
by hand too, and `release.py` publishes releases.

## Checks

Every push to a branch, or to a merge request's branch, builds `ttc` and
`ttcd` and runs TTC's unit tests on three platforms:

| Job                  | Runner                         | Also                       |
| -------------------- | ------------------------------ | -------------------------- |
| `check:linux-amd64`  | shared Docker, `os/linux`, `size/large` | static binaries that carry the GPU renderer |
| `check:linux-arm64`  | shared Docker, `os/linux-arm`  | the same, natively built    |
| `check:macos-arm64`  | the project's Mac, `os/macos-arm` | HVF platform tests, a `ttcd` that is the GPU renderer |

The tests are those of TTC's own packages, which need no privileges and
no GPU: the renderer's tests on a GPU skip where none renders. Their JUnit
reports show in merge requests, and the logs of failed tests are the job's
artifacts.

A test must not assume much of its host: Linux jobs run as root, in
containers whose pidfds may not poll, and the arm64 runner's pages are
64 KiB, which the ptrace platform cannot map.

Linux jobs run in `TTC_CI_LINUX_IMAGE` (default `ubuntu:22.04`), into which
`ci.sh setup` installs what the build needs and Bazel; the pipeline caches
Bazel's downloads and outputs, and apt's packages, per architecture. The
Mac runs one job at a time, with the shell executor, and its Bazel output
base persists between jobs; the Bazel server ends 15 minutes after a job.

## Daily releases

A pipeline of the default branch from a schedule, or one run by hand with
the variable `TTC_RELEASE=1`, releases when the branch changed since the
last release, other than in `g3doc/`, `website/` and Markdown files:

1. `release:plan` names the release `daily-YYYYMMDD-COMMIT` (UTC), or
   decides that there is none, and writes its notes: the commits since the
   last release.
2. `release:linux-amd64`, `release:linux-arm64` and `release:macos-arm64`
   build and test each platform's binaries, whose version is the release's
   tag, and archive them: `ttc-linux-amd64.zip`, `ttc-linux-arm64.zip` and
   `ttc-darwin-arm64.zip` each hold `ttc` and `ttcd`, to install side by
   side. macOS's `ttcd` is signed ad hoc with the Hypervisor entitlement,
   and its zip also holds the entitlements, `hvf.entitlements`, to sign it
   again. Each job unpacks its zip and runs what it holds. Without a release
   they end at once.
3. `release:publish` uploads the zips and their `SHA256SUMS` to the
   project's package registry, as package `ttc`, and makes the release, and
   its tag, with each file as a direct asset: the latest release's are at
   `<project>/-/releases/permalink/latest/downloads/<file>`.

It authenticates with the job's token, so the schedule's owner needs the
right to make tags, which protected tags may restrict.

To set it up, once: in Build > Pipeline schedules, schedule the default
branch daily. The project needs its package registry (Settings > General >
Visibility).

## By hand

From the repository's root:

```sh
tools/ci/ci.sh check          # build and unit tests, on Linux or macOS
sudo tools/ci/ci.sh setup     # Linux build dependencies, on Ubuntu
```

`release-plan` and `release.py` need a pipeline's variables.
