// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// gles31 checks what OpenGL ES 3.1 adds to 3.0, without a display, on EGL's
// surfaceless platform: compute shaders, of shader storage buffers, shared
// memory, atomic counters and images, of grids of their own and of a
// buffer's; memory barriers; indirect draws; stencil texturing;
// multisampled textures; and framebuffers of no attachments, of a fragment
// shader's storage buffer.
//
// Usage: gles31
// It prints the renderer and the version, and "gles31: ok" if every check
// passes.
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES3/gl31.h>
#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifndef EGL_PLATFORM_SURFACELESS_MESA
#define EGL_PLATFORM_SURFACELESS_MESA 0x31DD
#endif

enum { N = 256, W = 16, H = 16 };

static int failures;

static void die(const char *what) {
	fprintf(stderr, "gles31: %s (EGL error 0x%x, GL error 0x%x)\n", what, eglGetError(), glGetError());
	exit(1);
}

// fail reports a check that failed.
static void fail(const char *format, ...) {
	va_list ap;
	va_start(ap, format);
	fprintf(stderr, "gles31: ");
	vfprintf(stderr, format, ap);
	fprintf(stderr, "\n");
	va_end(ap);
	failures++;
}

static GLuint compile(GLenum type, const char *source) {
	GLuint s = glCreateShader(type);
	glShaderSource(s, 1, &source, NULL);
	glCompileShader(s);
	GLint ok;
	glGetShaderiv(s, GL_COMPILE_STATUS, &ok);
	if (!ok) {
		char log[2048];
		glGetShaderInfoLog(s, sizeof(log), NULL, log);
		fprintf(stderr, "gles31: compiling a shader: %s\n", log);
		exit(1);
	}
	return s;
}

// program links a program of a compute shader, of a vertex and a fragment
// shader if fragment is not NULL.
static GLuint program(GLenum type, const char *source, const char *fragment) {
	GLuint p = glCreateProgram();
	glAttachShader(p, compile(type, source));
	if (fragment) {
		glAttachShader(p, compile(GL_FRAGMENT_SHADER, fragment));
	}
	glLinkProgram(p);
	GLint ok;
	glGetProgramiv(p, GL_LINK_STATUS, &ok);
	if (!ok) {
		char log[2048];
		glGetProgramInfoLog(p, sizeof(log), NULL, log);
		fprintf(stderr, "gles31: linking a program: %s\n", log);
		exit(1);
	}
	return p;
}

// buffer returns a buffer of target, of size bytes of data, or of zeros.
static GLuint buffer(GLenum target, GLsizeiptr size, const void *data) {
	GLuint b;
	glGenBuffers(1, &b);
	glBindBuffer(target, b);
	glBufferData(target, size, data, GL_DYNAMIC_DRAW);
	if (!data) {
		void *p = glMapBufferRange(target, 0, size, GL_MAP_WRITE_BIT);
		memset(p, 0, (size_t)size);
		glUnmapBuffer(target);
	}
	return b;
}

// framebuffer returns a framebuffer of a texture of W by H of RGBA8, which
// it binds.
static GLuint framebuffer(GLuint *texture) {
	GLuint fb;
	glGenTextures(1, texture);
	glBindTexture(GL_TEXTURE_2D, *texture);
	glTexStorage2D(GL_TEXTURE_2D, 1, GL_RGBA8, W, H);
	glGenFramebuffers(1, &fb);
	glBindFramebuffer(GL_FRAMEBUFFER, fb);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, *texture, 0);
	if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) {
		die("completing a framebuffer");
	}
	glViewport(0, 0, W, H);
	return fb;
}

static const char *compute_source =
    "#version 310 es\n"
    "layout(local_size_x = 64) in;\n"
    "layout(std430, binding = 0) readonly buffer In { uint words[]; } src;\n"
    "layout(std430, binding = 1) writeonly buffer Out { uint words[]; } dst;\n"
    "layout(binding = 0, offset = 0) uniform atomic_uint invocations;\n"
    "layout(binding = 0, rgba8) writeonly uniform highp image2D image;\n"
    "shared uint reversed[64];\n"
    "void main() {\n"
    "  uint i = gl_LocalInvocationID.x;\n"
    "  uint g = gl_GlobalInvocationID.x;\n"
    "  reversed[63u - i] = src.words[g];\n"
    "  barrier();\n"
    "  dst.words[g] = reversed[i] * 2u;\n"
    "  atomicCounterIncrement(invocations);\n"
    "  imageStore(image, ivec2(int(g % 16u), int(g / 16u)),\n"
    "             vec4(float(g % 16u) / 15.0, float(g / 16u) / 15.0, 0.0, 1.0));\n"
    "}\n";

// check_compute dispatches the compute shader's 4 workgroups of 64, of
// their counts and of a buffer's: each reverses its workgroup's words of a
// buffer, doubled, to another, counts itself, and stores a texel of an
// image of its index.
static void check_compute(void) {
	GLuint p = program(GL_COMPUTE_SHADER, compute_source, NULL);
	uint32_t words[N];
	for (int i = 0; i < N; i++) {
		words[i] = 1000 + i;
	}
	GLuint src = buffer(GL_SHADER_STORAGE_BUFFER, sizeof(words), words);
	GLuint dst = buffer(GL_SHADER_STORAGE_BUFFER, sizeof(words), NULL);
	GLuint counter = buffer(GL_ATOMIC_COUNTER_BUFFER, 4, NULL);
	static const GLuint grid[4] = {0, 4, 1, 1};
	GLuint grids = buffer(GL_DISPATCH_INDIRECT_BUFFER, sizeof(grid), grid);
	GLuint image;
	GLuint fb = framebuffer(&image);
	glBindBufferBase(GL_SHADER_STORAGE_BUFFER, 0, src);
	glBindBufferBase(GL_SHADER_STORAGE_BUFFER, 1, dst);
	glBindBufferBase(GL_ATOMIC_COUNTER_BUFFER, 0, counter);
	glBindImageTexture(0, image, 0, GL_FALSE, 0, GL_WRITE_ONLY, GL_RGBA8);
	glUseProgram(p);
	glDispatchCompute(N / 64, 1, 1);
	glMemoryBarrier(GL_SHADER_STORAGE_BARRIER_BIT);
	glBindBuffer(GL_DISPATCH_INDIRECT_BUFFER, grids);
	glDispatchComputeIndirect(4);
	glMemoryBarrier(GL_BUFFER_UPDATE_BARRIER_BIT | GL_ATOMIC_COUNTER_BARRIER_BIT | GL_FRAMEBUFFER_BARRIER_BIT);
	if (glGetError() != GL_NO_ERROR) {
		die("dispatching");
	}

	glBindBuffer(GL_SHADER_STORAGE_BUFFER, dst);
	const uint32_t *out = glMapBufferRange(GL_SHADER_STORAGE_BUFFER, 0, sizeof(words), GL_MAP_READ_BIT);
	for (int i = 0; i < N; i++) {
		uint32_t want = 2 * (1000 + i / 64 * 64 + 63 - i % 64);
		if (out[i] != want) {
			fail("word %d of the compute shader's buffer is %u, want %u", i, out[i], want);
			break;
		}
	}
	glUnmapBuffer(GL_SHADER_STORAGE_BUFFER);
	glBindBuffer(GL_ATOMIC_COUNTER_BUFFER, counter);
	const uint32_t *count = glMapBufferRange(GL_ATOMIC_COUNTER_BUFFER, 0, 4, GL_MAP_READ_BIT);
	if (*count != 2 * N) {
		fail("the atomic counter counted %u invocations, want %u", *count, 2 * N);
	}
	glUnmapBuffer(GL_ATOMIC_COUNTER_BUFFER);
	static uint8_t pixels[H][W][4];
	glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pixels);
	for (int y = 0; y < H; y++) {
		for (int x = 0; x < W; x++) {
			const uint8_t *px = pixels[y][x];
			if (px[0] != x * 17 || px[1] != y * 17 || px[2] != 0 || px[3] != 255) {
				fail("texel (%d, %d) of the compute shader's image is %u,%u,%u,%u", x, y, px[0], px[1], px[2], px[3]);
				x = W, y = H;
			}
		}
	}
	glDeleteFramebuffers(1, &fb);
}

static const char *color_vertex =
    "#version 310 es\n"
    "layout(location = 0) in vec2 position;\n"
    "void main() { gl_Position = vec4(position, 0.0, 1.0); }\n";

static const char *color_fragment =
    "#version 310 es\n"
    "precision mediump float;\n"
    "uniform vec4 color;\n"
    "out vec4 fragColor;\n"
    "void main() { fragColor = color; }\n";

// expect checks the pixel of the bound framebuffer at x, y.
static void expect(const char *what, int x, int y, uint8_t r, uint8_t g, uint8_t b, uint8_t a) {
	uint8_t px[4];
	glReadPixels(x, y, 1, 1, GL_RGBA, GL_UNSIGNED_BYTE, px);
	if (px[0] != r || px[1] != g || px[2] != b || px[3] != a) {
		fail("%s: pixel (%d, %d) is %u,%u,%u,%u, want %u,%u,%u,%u", what, x, y, px[0], px[1], px[2], px[3], r, g, b, a);
	}
}

// check_indirect_draws draws the lower left triangle of the framebuffer of
// a command of a buffer's, and the upper right one of indexes, of another.
static void check_indirect_draws(void) {
	GLuint p = program(GL_VERTEX_SHADER, color_vertex, color_fragment);
	GLuint texture;
	GLuint fb = framebuffer(&texture);
	GLuint vao;
	glGenVertexArrays(1, &vao);
	glBindVertexArray(vao);
	static const GLfloat vertices[] = {-1, -1, 1, -1, -1, 1, 1, 1, -1, 1, 1, -1};
	buffer(GL_ARRAY_BUFFER, sizeof(vertices), vertices);
	glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 0, 0);
	glEnableVertexAttribArray(0);
	static const GLushort indexes[] = {0, 0, 0, 3, 4, 5};
	buffer(GL_ELEMENT_ARRAY_BUFFER, sizeof(indexes), indexes);
	// DrawArraysIndirectCommand, and DrawElementsIndirectCommand.
	static const GLuint commands[] = {3, 1, 0, 0, 3, 1, 3, 0, 0};
	buffer(GL_DRAW_INDIRECT_BUFFER, sizeof(commands), commands);
	glClearColor(0, 0, 0, 1);
	glClear(GL_COLOR_BUFFER_BIT);
	glUseProgram(p);
	glUniform4f(glGetUniformLocation(p, "color"), 1, 0, 0, 1);
	glDrawArraysIndirect(GL_TRIANGLES, (const void *)0);
	glUniform4f(glGetUniformLocation(p, "color"), 0, 1, 0, 1);
	glDrawElementsIndirect(GL_TRIANGLES, GL_UNSIGNED_SHORT, (const void *)16);
	if (glGetError() != GL_NO_ERROR) {
		die("drawing indirectly");
	}
	expect("the first indirect draw", 2, 2, 255, 0, 0, 255);
	expect("the second indirect draw", W - 3, H - 3, 0, 255, 0, 255);
	glBindVertexArray(0);
	glDeleteVertexArrays(1, &vao);
	glDeleteFramebuffers(1, &fb);
}

static const char *fullscreen_vertex =
    "#version 310 es\n"
    "void main() {\n"
    "  vec2 p = vec2(gl_VertexID == 1 ? 3.0 : -1.0, gl_VertexID == 2 ? 3.0 : -1.0);\n"
    "  gl_Position = vec4(p, 0.0, 1.0);\n"
    "}\n";

static const char *stencil_fragment =
    "#version 310 es\n"
    "precision mediump float;\n"
    "uniform highp usampler2D stencil;\n"
    "uniform highp sampler2D depth;\n"
    "out vec4 fragColor;\n"
    "void main() {\n"
    "  ivec2 p = ivec2(gl_FragCoord.xy);\n"
    "  fragColor = vec4(float(texelFetch(stencil, p, 0).r) / 255.0, texelFetch(depth, p, 0).r, 0.0, 1.0);\n"
    "}\n";

// check_stencil_texturing samples the stencil, and the depth, of a depth
// and stencil texture.
static void check_stencil_texturing(void) {
	GLuint zs;
	glGenTextures(1, &zs);
	glBindTexture(GL_TEXTURE_2D, zs);
	glTexStorage2D(GL_TEXTURE_2D, 1, GL_DEPTH24_STENCIL8, W, H);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
	GLuint fb;
	glGenFramebuffers(1, &fb);
	glBindFramebuffer(GL_FRAMEBUFFER, fb);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_DEPTH_STENCIL_ATTACHMENT, GL_TEXTURE_2D, zs, 0);
	glClearBufferfi(GL_DEPTH_STENCIL, 0, 0.5f, 0x5a);
	glDeleteFramebuffers(1, &fb);
	glTexParameteri(GL_TEXTURE_2D, GL_DEPTH_STENCIL_TEXTURE_MODE, GL_STENCIL_INDEX);
	// Of the texture's stencil, of unit 0, and of another texture's depth,
	// of unit 1.
	GLuint depth;
	glGenTextures(1, &depth);
	glActiveTexture(GL_TEXTURE1);
	glBindTexture(GL_TEXTURE_2D, depth);
	glTexStorage2D(GL_TEXTURE_2D, 1, GL_DEPTH_COMPONENT24, W, H);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
	glGenFramebuffers(1, &fb);
	glBindFramebuffer(GL_FRAMEBUFFER, fb);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_DEPTH_ATTACHMENT, GL_TEXTURE_2D, depth, 0);
	glClearBufferfv(GL_DEPTH, 0, (const GLfloat[]){0.5f});
	glDeleteFramebuffers(1, &fb);
	glActiveTexture(GL_TEXTURE0);

	GLuint p = program(GL_VERTEX_SHADER, fullscreen_vertex, stencil_fragment);
	GLuint texture;
	fb = framebuffer(&texture);
	glBindTexture(GL_TEXTURE_2D, zs);
	glUseProgram(p);
	glUniform1i(glGetUniformLocation(p, "stencil"), 0);
	glUniform1i(glGetUniformLocation(p, "depth"), 1);
	GLuint vao;
	glGenVertexArrays(1, &vao);
	glBindVertexArray(vao);
	glDrawArrays(GL_TRIANGLES, 0, 3);
	if (glGetError() != GL_NO_ERROR) {
		die("sampling a stencil");
	}
	// 0.5 of 255, which GPUs round either way.
	uint8_t px[4];
	glReadPixels(W / 2, H / 2, 1, 1, GL_RGBA, GL_UNSIGNED_BYTE, px);
	if (px[0] != 0x5a || px[1] < 127 || px[1] > 128) {
		fail("the stencil and the depth sampled: %u,%u, want %u,128", px[0], px[1], 0x5a);
	}
	glActiveTexture(GL_TEXTURE0);
	glBindVertexArray(0);
	glDeleteVertexArrays(1, &vao);
	glDeleteFramebuffers(1, &fb);
}

static const char *multisample_fragment =
    "#version 310 es\n"
    "precision mediump float;\n"
    "uniform highp sampler2DMS samples;\n"
    "out vec4 fragColor;\n"
    "void main() {\n"
    "  vec4 c = texelFetch(samples, ivec2(gl_FragCoord.xy), 3);\n"
    "  fragColor = vec4(textureSize(samples) == ivec2(16, 16) ? 1.0 : 0.0, c.gba);\n"
    "}\n";

// check_multisample_textures fetches a sample of a multisampled texture,
// cleared, and checks the positions of its samples.
static void check_multisample_textures(void) {
	GLint samples;
	glGetInternalformativ(GL_TEXTURE_2D_MULTISAMPLE, GL_RGBA8, GL_SAMPLES, 1, &samples);
	if (samples < 4) {
		fail("textures of RGBA8 of %d samples at most", samples);
		return;
	}
	GLuint ms;
	glGenTextures(1, &ms);
	glBindTexture(GL_TEXTURE_2D_MULTISAMPLE, ms);
	glTexStorage2DMultisample(GL_TEXTURE_2D_MULTISAMPLE, 4, GL_RGBA8, W, H, GL_TRUE);
	GLuint fb;
	glGenFramebuffers(1, &fb);
	glBindFramebuffer(GL_FRAMEBUFFER, fb);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D_MULTISAMPLE, ms, 0);
	glClearColor(0, 1, 0, 1);
	glClear(GL_COLOR_BUFFER_BIT);
	for (int i = 0; i < 4; i++) {
		GLfloat pos[2];
		glGetMultisamplefv(GL_SAMPLE_POSITION, i, pos);
		if (pos[0] < 0 || pos[0] > 1 || pos[1] < 0 || pos[1] > 1) {
			fail("sample %d's position is %f,%f", i, pos[0], pos[1]);
		}
	}
	glDeleteFramebuffers(1, &fb);
	GLuint p = program(GL_VERTEX_SHADER, fullscreen_vertex, multisample_fragment);
	GLuint texture;
	fb = framebuffer(&texture);
	glUseProgram(p);
	glUniform1i(glGetUniformLocation(p, "samples"), 0);
	glActiveTexture(GL_TEXTURE0);
	glBindTexture(GL_TEXTURE_2D_MULTISAMPLE, ms);
	GLuint vao;
	glGenVertexArrays(1, &vao);
	glBindVertexArray(vao);
	glClearColor(0, 0, 0, 0);
	glClear(GL_COLOR_BUFFER_BIT);
	glDrawArrays(GL_TRIANGLES, 0, 3);
	if (glGetError() != GL_NO_ERROR) {
		die("fetching samples");
	}
	expect("a multisampled texture's sample", W / 2, H / 2, 255, 255, 0, 255);
	glBindVertexArray(0);
	glDeleteVertexArrays(1, &vao);
	glDeleteFramebuffers(1, &fb);
}

static const char *counting_fragment =
    "#version 310 es\n"
    "precision mediump float;\n"
    "layout(std430, binding = 0) buffer Count { highp uint fragments; };\n"
    "void main() { atomicAdd(fragments, 1u); }\n";

// check_no_attachments draws a fullscreen triangle to a framebuffer of no
// attachments, of W by H, whose fragments a fragment shader's storage
// buffer counts.
static void check_no_attachments(void) {
	GLint blocks;
	glGetIntegerv(GL_MAX_FRAGMENT_SHADER_STORAGE_BLOCKS, &blocks);
	if (blocks < 1) {
		fail("fragment shaders of %d storage buffers", blocks);
		return;
	}
	GLuint fb;
	glGenFramebuffers(1, &fb);
	glBindFramebuffer(GL_FRAMEBUFFER, fb);
	glFramebufferParameteri(GL_FRAMEBUFFER, GL_FRAMEBUFFER_DEFAULT_WIDTH, W);
	glFramebufferParameteri(GL_FRAMEBUFFER, GL_FRAMEBUFFER_DEFAULT_HEIGHT, H);
	if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) {
		die("completing a framebuffer of no attachments");
	}
	glViewport(0, 0, W, H);
	GLuint count = buffer(GL_SHADER_STORAGE_BUFFER, 4, NULL);
	glBindBufferBase(GL_SHADER_STORAGE_BUFFER, 0, count);
	GLuint p = program(GL_VERTEX_SHADER, fullscreen_vertex, counting_fragment);
	glUseProgram(p);
	GLuint vao;
	glGenVertexArrays(1, &vao);
	glBindVertexArray(vao);
	glDrawArrays(GL_TRIANGLES, 0, 3);
	glMemoryBarrier(GL_BUFFER_UPDATE_BARRIER_BIT);
	if (glGetError() != GL_NO_ERROR) {
		die("drawing to no attachments");
	}
	glBindBuffer(GL_SHADER_STORAGE_BUFFER, count);
	const uint32_t *n = glMapBufferRange(GL_SHADER_STORAGE_BUFFER, 0, 4, GL_MAP_READ_BIT);
	if (*n != W * H) {
		fail("the fragments of no attachments: %u, want %u", *n, W * H);
	}
	glUnmapBuffer(GL_SHADER_STORAGE_BUFFER);
	glBindVertexArray(0);
	glDeleteVertexArrays(1, &vao);
	glDeleteFramebuffers(1, &fb);
}

int main(void) {
	EGLDisplay dpy = eglGetPlatformDisplay(EGL_PLATFORM_SURFACELESS_MESA, EGL_DEFAULT_DISPLAY, NULL);
	if (dpy == EGL_NO_DISPLAY || !eglInitialize(dpy, NULL, NULL)) {
		die("initializing EGL's surfaceless display");
	}
	if (!eglBindAPI(EGL_OPENGL_ES_API)) {
		die("binding OpenGL ES");
	}
	// The surfaceless platform's configs have no windows.
	static const EGLint config_attribs[] = {EGL_SURFACE_TYPE, EGL_PBUFFER_BIT, EGL_RENDERABLE_TYPE,
	                                        EGL_OPENGL_ES3_BIT, EGL_NONE};
	EGLConfig config;
	EGLint configs;
	if (!eglChooseConfig(dpy, config_attribs, &config, 1, &configs) || configs < 1) {
		die("choosing a config");
	}
	static const EGLint context_attribs[] = {EGL_CONTEXT_MAJOR_VERSION, 3, EGL_CONTEXT_MINOR_VERSION, 1, EGL_NONE};
	EGLContext ctx = eglCreateContext(dpy, config, EGL_NO_CONTEXT, context_attribs);
	if (ctx == EGL_NO_CONTEXT || !eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, ctx)) {
		die("creating a context of OpenGL ES 3.1");
	}
	printf("gles31: renderer %s, %s\n", glGetString(GL_RENDERER), glGetString(GL_VERSION));
	GLint major, minor;
	glGetIntegerv(GL_MAJOR_VERSION, &major);
	glGetIntegerv(GL_MINOR_VERSION, &minor);
	if (major < 3 || (major == 3 && minor < 1)) {
		fail("OpenGL ES %d.%d", major, minor);
	}

	check_compute();
	check_indirect_draws();
	check_stencil_texturing();
	check_multisample_textures();
	check_no_attachments();
	if (failures) {
		fprintf(stderr, "gles31: %d checks failed\n", failures);
		return 1;
	}
	printf("gles31: ok\n");
	eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
	eglDestroyContext(dpy, ctx);
	eglTerminate(dpy);
	return 0;
}
