// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package bundle gives ttcd, which imports it, an Info.plist on macOS, in
// the binary's __TEXT,__info_plist section: its bundle identifier,
// com.nvidia.ttcd, names the caches that Apple's frameworks keep for ttcd
// alone, under the user's cache directory, such as Metal's compiled shaders
// and its compiler's precompiled headers, which ttcd's GPU renderer reads
// and writes (see seatbelt.BundleCacheDir). Without one, the frameworks
// keep the caches that they share among all of the user's programs of no
// identifier, which the renderer's sandbox denies.
package bundle
