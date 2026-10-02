// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

#version 450
// Two triangles covering [-0.5, 0.5]^2: a square whose edges lie on pixel
// boundaries of a 64x64 target.
void main() {
	const vec2 corners[6] = vec2[](vec2(-0.5, -0.5), vec2(0.5, -0.5), vec2(0.5, 0.5),
	                               vec2(-0.5, -0.5), vec2(0.5, 0.5), vec2(-0.5, 0.5));
	gl_Position = vec4(corners[gl_VertexIndex], 0.0, 1.0);
}
