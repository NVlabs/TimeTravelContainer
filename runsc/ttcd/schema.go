// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sort"

	"gvisor.dev/gvisor/pkg/state"
)

// writeStateSchema writes, as JSON, the fields that this build saves of every
// savable type and the types they are saved as, by the type's name: the shape
// of what its dehydrations and checkpoints hold. One type per line, sorted,
// so that two releases' schemas compare line by line. A release's schema,
// kept in schemas/, is what later builds must load (see
// pkg/state/migrate.go).
func writeStateSchema(w io.Writer) int {
	schema := state.Schema()
	names := make([]string, 0, len(schema))
	for name := range schema {
		names = append(names, name)
	}
	sort.Strings(names)
	out := bufio.NewWriter(w)
	out.WriteString("{\n")
	for i, name := range names {
		key, _ := json.Marshal(name)
		value, err := json.Marshal(schema[name])
		if err != nil {
			os.Stderr.WriteString("ttcd: writing the state schema: " + err.Error() + "\n")
			return 1
		}
		out.WriteString(" ")
		out.Write(key)
		out.WriteString(": ")
		out.Write(value)
		if i < len(names)-1 {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	out.WriteString("}\n")
	if err := out.Flush(); err != nil {
		os.Stderr.WriteString("ttcd: writing the state schema: " + err.Error() + "\n")
		return 1
	}
	return 0
}
