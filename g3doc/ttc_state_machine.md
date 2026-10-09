# TimeTravelContainer branch state machine

TTC lifecycle state belongs to a branch. A container family is a namespace for
related branches and shared backing objects; its displayed state is only a
summary derived from its branches. `main` is an ordinary branch name, and
parent links are presentation metadata for `ttc ls --tree` rather than runtime
ownership.

## Stable branch states

| State | Meaning |
| --- | --- |
| `created` | Filesystem and configuration exist, but the branch has not started. |
| `running` | The branch has live guest tasks. |
| `paused` | Guest tasks and their runtime state remain resident but do not execute. |
| `dehydrated` | The complete branch state is durable and no Sentry process is required for it. |
| `exited` | The execution state is gone; the branch filesystem, identity, configuration, and logs remain and can be started fresh. |

`creating`, `dehydrating`, `rehydrating`, and `restarting` are transient
states. `failed` and `dead` report an interrupted or unrecoverable operation.
In a cluster, `corrupted` marks a branch whose live runtime, or the only copy
of state it needs, was lost; only `rm` is permitted on it. Legacy `stopped`
records are presented as `exited`.

## Operations and transitions

| Command | Target scope | Transition |
| --- | --- | --- |
| `start FAMILY@BRANCH` | Branch | `created`/`exited` → `running` |
| `pause TARGET` | Branch or family | `running` → `paused`; a family target pauses its running branches |
| `resume TARGET` | Branch or family | `paused` → `running`; a family target resumes its paused branches |
| `dehydrate TARGET` | Branch or family | `running`/`paused` → `dehydrating` → `dehydrated` |
| `rehydrate FAMILY@BRANCH` | Branch | `dehydrated` → `rehydrating` → its pre-dehydration `running` or `paused` state |
| `stop TARGET` | Branch or family | `running` → graceful exit/escalation → `exited`; `paused`/`dehydrated` → direct discard → `exited` |
| `restart FAMILY@BRANCH` | Branch | Stop the selected branch, then start it fresh |
| `rm TARGET` | Branch or family | Remove the selected branch and its private artifacts, or remove the entire family |

`stop` is idempotent for `created` and `exited` branches. Stopping a paused or
dehydrated branch does not resume it or execute guest instructions. `rm`
requires `-f` for a running or paused branch. Removing a branch splices its
children to its display parent, and its branch name can immediately be reused.
For a container started with `--rm`, automatic removal applies to the exiting
branch; forked peers survive. The whole family and its anonymous volumes are
removed automatically only when that branch is the family's sole member.

A family target expands to its current branches. Family-wide operations are
performed branch by branch and are not transactional: an error after some
branches have completed is reported without undoing those completed changes.

Starting or restarting an exited branch preserves active peers. TTC briefly
starts only the selected branch in a temporary paused bootstrap, imports that
never-executed branch into the family's existing Sentry, and then resumes it.
Running and paused peers are neither serialized nor replaced, so the work is
bounded by the new branch's bootstrap state rather than their memory size.

If the branch used internally by runsc for process bookkeeping exits naturally,
TTC transfers that bookkeeping role to another live branch. This internal role
does not affect names, ancestry, copy-on-write ownership, or branch lifetime.
