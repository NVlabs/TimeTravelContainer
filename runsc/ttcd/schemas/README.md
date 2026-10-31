<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Saved schemas of TTC releases

A build of TTC loads the dehydrated branches and checkpoints of the builds
before it (pkg/state/migrate.go). Each file here is the shape of what one of
those builds saves: every savable type, by name, with the fields it saves and
the types they are saved as. `TestThisBuildLoadsEveryReleasesImages` in this
package checks that the current build loads every one of them.

- `daily-20261023.json`: pre-merge master, a9108907a (the dailies through
  20261023), the last build before TTC's merge of upstream gVisor through
  2026-10-28.

## When the test fails

A change to a savable struct, TTC's own or a merge's, fails the test until the
type's package says how the type loads from each older shape: a
`state.Migration` in its `state_migrations.go`. A migration names the older
fields exactly, and accounts for every difference: renamed fields, dropped
ones, added ones that are right at zero (`Zero`), and added ones that a fix
sets (`Set`, with `Fix` or `FixAfterLoad`, and `Old` for the older values a
fix needs). A type that TTC never saves can be refused instead (`Refuse`).
Write down why each choice is right; the existing migrations show how.

Prefer a new field name to a new meaning for an old one: the image records
field names, so a field that changes type or meaning under the same name is
caught only here, by its saved type.

## At a release

Add the release's schema, named after the release:

```shell
ttcd _state-schema > runsc/ttcd/schemas/RELEASE.json
```

It writes one type per line, sorted, so that two releases' schemas compare
line by line.
