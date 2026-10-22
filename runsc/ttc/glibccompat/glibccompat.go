// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package glibccompat binds the C library functions that the GPU renderer
// calls, through Go's runtime, its own C code and Mesa's Zink, to versions
// that every glibc since 2.17 has, so that the renderer, which ttcd carries, runs on a
// system of any glibc since: a binary that a newer glibc links needs that
// one. Its C files define those functions anew, for the binary alone. It has
// no Go API.
package glibccompat
