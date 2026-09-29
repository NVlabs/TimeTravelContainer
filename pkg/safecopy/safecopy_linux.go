// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package safecopy

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sighandling"
)

func initializePlatform() error {
	if err := sighandling.ReplaceSignalHandler(unix.SIGSEGV, addrOfSignalHandler(), &savedSigSegVHandler); err != nil {
		return fmt.Errorf("setting SIGSEGV handler: %w", err)
	}
	if err := sighandling.ReplaceSignalHandler(unix.SIGBUS, addrOfSignalHandler(), &savedSigBusHandler); err != nil {
		return fmt.Errorf("setting SIGBUS handler: %w", err)
	}
	return nil
}
