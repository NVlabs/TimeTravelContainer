# TTC's CI

`.gitlab-ci.yml` runs TTC's CI on GitLab, and `.github/workflows/ttc-check.yml`
and `ttc-release.yml` run the same on GitHub's hosted runners (see "On
GitHub"). `ci.sh` holds their steps, which run by hand too, and `release.py`
publishes releases on either.

## Checks

Every push to a branch, or to a merge request's branch, builds `ttc` and
`ttcd` and runs TTC's unit tests on three platforms:

| Job                  | Runner                         | Also                       |
| -------------------- | ------------------------------ | -------------------------- |
| `check:linux-amd64`  | shared Docker, `os/linux`, `size/large` | static binaries that carry the GPU renderer |
| `check:linux-arm64`  | shared Docker, `os/linux-arm`  | the same, natively built    |
| `check:macos-arm64`  | the project's Mac, `os/macos-arm` | HVF platform tests, a `ttcd` that is the GPU renderer and loads only Apple's libraries |

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
   side, with `LICENSE` and `THIRD_PARTY_NOTICES.md`. macOS's `ttcd` is
   signed ad hoc with the Hypervisor entitlement, and its zip also holds the
   entitlements, `hvf.entitlements`, to sign it again. Each job unpacks its
   zip and runs what it holds. Without a release they end at once.
3. `release:publish` uploads the zips and their `SHA256SUMS` to the
   project's package registry, as package `ttc`, and makes the release, and
   its tag, with each file as a direct asset: the latest release's are at
   `<project>/-/releases/permalink/latest/downloads/<file>`.

It authenticates with the job's token, so the schedule's owner needs the
right to make tags, which protected tags may restrict.

Daily releases expire: after publishing, and daily when there is nothing to
release, `release:publish` deletes the daily releases older than
`TTC_RELEASE_KEEP_DAYS` days (a CI/CD variable; 30 if it is unset), the
newest excepted, with their packages, and their tags where the job's token
may delete tags. The jobs' artifacts expire too: the failed tests' logs after
a week, and the release jobs' archives after a month.

To set it up, once: in Build > Pipeline schedules, schedule the default
branch daily. The project needs its package registry (Settings > General >
Visibility).

## On GitHub

The workflows do what the pipeline does, on GitHub's hosted runners:
`ubuntu-24.04` and `ubuntu-24.04-arm`, whose jobs run in `ubuntu:22.04` as
root, as GitLab's do, once they have deleted the toolchains the runner comes
with for disk space; and `macos-15`, whose virtual machines have no
Hypervisor.framework, so that the HVF platform's tests skip there.
`actions/cache` keeps Bazel's downloads and outputs between runs, 3 GB of
them per platform, within the 10 GB that GitHub keeps per repository.

- `ttc-check.yml` checks each push to the default branch, and each pull
  request to it, on the three platforms. A newer push cancels an older one's
  checks. The logs of failed tests are the run's artifacts for a week.
- `ttc-release.yml` releases daily, at 03:17 UTC, or when run by hand
  (Actions > TTC daily release > Run workflow) on the default branch, as
  `daily-YYYYMMDD-COMMIT`: the same zips and `SHA256SUMS`, each an asset of
  the release, which is the repository's latest, so that the latest's are at
  `<repository>/releases/latest/download/<file>`. The platforms' archives are
  the run's artifacts for a week. Daily releases older than
  `TTC_RELEASE_KEEP_DAYS` days (a repository variable, Settings > Secrets and
  variables > Actions > Variables; 30 if it is unset) expire, the newest
  excepted, with their tags.

The release job authenticates with the workflow's token, which the workflow
gives `contents: write`. GitHub turns off the schedules of a repository that
nothing has been pushed to for 60 days.

## By hand

From the repository's root:

```sh
tools/ci/ci.sh check          # build and unit tests, on Linux or macOS
sudo tools/ci/ci.sh setup     # Linux build dependencies, on Ubuntu
```

`release-plan` and `release.py` need a pipeline's, or a workflow's,
variables.
