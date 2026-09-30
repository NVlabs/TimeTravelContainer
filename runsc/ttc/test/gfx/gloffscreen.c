// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// gloffscreen checks OpenGL ES without a display, on EGL's surfaceless
// platform: it clears a 64x64 framebuffer to blue, draws a square of a
// uniform's color in its left half, and a 2x2 texture, sampled nearest,
// over its right half. The shapes' edges lie on pixel boundaries and texels
// cover whole pixels, so every pixel is determined, and the program checks
// each.
//
// Usage: gloffscreen [ITERATIONS [DELAY_MS]]
// It prints the renderer, and "gloffscreen: ok checksum=..." if every check
// passes. With ITERATIONS, it repeats the draw and the checks in one
// context, DELAY_MS apart, and prints "gloffscreen: iteration N ok" after
// each.
#include <EGL/egl.h>
#include <EGL/eglext.h>
#include <GLES2/gl2.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#ifndef EGL_PLATFORM_SURFACELESS_MESA
#define EGL_PLATFORM_SURFACELESS_MESA 0x31DD
#endif

enum { W = 64, H = 64 };

static void die(const char *what) {
	fprintf(stderr, "gloffscreen: %s (EGL error 0x%x, GL error 0x%x)\n", what,
	        eglGetError(), glGetError());
	exit(1);
}

static GLuint compile(GLenum type, const char *source) {
	GLuint s = glCreateShader(type);
	glShaderSource(s, 1, &source, NULL);
	glCompileShader(s);
	GLint ok;
	glGetShaderiv(s, GL_COMPILE_STATUS, &ok);
	if (!ok) {
		char log[1024];
		glGetShaderInfoLog(s, sizeof(log), NULL, log);
		fprintf(stderr, "gloffscreen: compiling a shader: %s\n", log);
		exit(1);
	}
	return s;
}

static GLuint program(const char *vertex, const char *fragment) {
	GLuint p = glCreateProgram();
	glAttachShader(p, compile(GL_VERTEX_SHADER, vertex));
	glAttachShader(p, compile(GL_FRAGMENT_SHADER, fragment));
	glBindAttribLocation(p, 0, "position");
	glLinkProgram(p);
	GLint ok;
	glGetProgramiv(p, GL_LINK_STATUS, &ok);
	if (!ok) {
		die("linking a program");
	}
	return p;
}

static const char *vertex_source =
    "attribute vec2 position;\n"
    "varying vec2 uv;\n"
    "void main() {\n"
    "  uv = vec2(position.x * 2.0, position.y + 1.0) * 0.5;\n"
    "  gl_Position = vec4(position, 0.0, 1.0);\n"
    "}\n";

static const char *color_source =
    "precision mediump float;\n"
    "uniform vec4 color;\n"
    "void main() { gl_FragColor = color; }\n";

static const char *texture_source =
    "precision mediump float;\n"
    "uniform sampler2D tex;\n"
    "varying vec2 uv;\n"
    "void main() { gl_FragColor = texture2D(tex, uv); }\n";

// The texture's texels, RGBA, bottom row first.
static const uint8_t texels[2][2][4] = {
    {{255, 255, 0, 255}, {0, 255, 255, 255}},
    {{255, 0, 255, 255}, {255, 255, 255, 255}},
};

// expected returns the color of pixel (x, y), y from the bottom.
static const uint8_t *expected(int x, int y) {
	static const uint8_t blue[4] = {0, 0, 255, 255};
	static const uint8_t red[4] = {255, 0, 0, 255};
	if (x < W / 2) {
		// The square spans the left half's middle half.
		if (x >= W / 8 && x < 3 * W / 8 && y >= H / 4 && y < 3 * H / 4) {
			return red;
		}
		return blue;
	}
	return texels[y / (H / 2)][(x - W / 2) / (W / 4)];
}

int main(int argc, char **argv) {
	if (argc > 3) {
		fprintf(stderr, "usage: gloffscreen [ITERATIONS [DELAY_MS]]\n");
		return 2;
	}
	long iterations = argc > 1 ? atol(argv[1]) : 1;
	long delay_ms = argc > 2 ? atol(argv[2]) : 0;

	EGLDisplay dpy = eglGetPlatformDisplay(EGL_PLATFORM_SURFACELESS_MESA, EGL_DEFAULT_DISPLAY, NULL);
	if (dpy == EGL_NO_DISPLAY || !eglInitialize(dpy, NULL, NULL)) {
		die("initializing EGL's surfaceless display");
	}
	if (!eglBindAPI(EGL_OPENGL_ES_API)) {
		die("binding OpenGL ES");
	}
	// The surfaceless platform's configs have no windows.
	static const EGLint config_attribs[] = {EGL_SURFACE_TYPE, EGL_PBUFFER_BIT, EGL_RENDERABLE_TYPE,
	                                        EGL_OPENGL_ES2_BIT, EGL_NONE};
	EGLConfig config;
	EGLint configs;
	if (!eglChooseConfig(dpy, config_attribs, &config, 1, &configs) || configs < 1) {
		die("choosing a config");
	}
	static const EGLint context_attribs[] = {EGL_CONTEXT_CLIENT_VERSION, 2, EGL_NONE};
	EGLContext ctx = eglCreateContext(dpy, config, EGL_NO_CONTEXT, context_attribs);
	if (ctx == EGL_NO_CONTEXT || !eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, ctx)) {
		die("creating a context");
	}
	printf("gloffscreen: renderer %s, %s\n", glGetString(GL_RENDERER), glGetString(GL_VERSION));

	GLuint target, fb;
	glGenTextures(1, &target);
	glBindTexture(GL_TEXTURE_2D, target);
	glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, W, H, 0, GL_RGBA, GL_UNSIGNED_BYTE, NULL);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
	glGenFramebuffers(1, &fb);
	glBindFramebuffer(GL_FRAMEBUFFER, fb);
	glFramebufferTexture2D(GL_FRAMEBUFFER, GL_COLOR_ATTACHMENT0, GL_TEXTURE_2D, target, 0);
	if (glCheckFramebufferStatus(GL_FRAMEBUFFER) != GL_FRAMEBUFFER_COMPLETE) {
		die("completing the framebuffer");
	}

	GLuint tex;
	glGenTextures(1, &tex);
	glBindTexture(GL_TEXTURE_2D, tex);
	glTexImage2D(GL_TEXTURE_2D, 0, GL_RGBA, 2, 2, 0, GL_RGBA, GL_UNSIGNED_BYTE, texels);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MIN_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_MAG_FILTER, GL_NEAREST);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_S, GL_CLAMP_TO_EDGE);
	glTexParameteri(GL_TEXTURE_2D, GL_TEXTURE_WRAP_T, GL_CLAMP_TO_EDGE);

	GLuint color_program = program(vertex_source, color_source);
	GLuint texture_program = program(vertex_source, texture_source);
	// Two triangle strips: the square, and the right half.
	static const GLfloat vertices[] = {
	    -0.75f, -0.5f, -0.25f, -0.5f, -0.75f, 0.5f, -0.25f, 0.5f,
	    0.0f, -1.0f, 1.0f, -1.0f, 0.0f, 1.0f, 1.0f, 1.0f,
	};
	GLuint vbo;
	glGenBuffers(1, &vbo);
	glBindBuffer(GL_ARRAY_BUFFER, vbo);
	glBufferData(GL_ARRAY_BUFFER, sizeof(vertices), vertices, GL_STATIC_DRAW);
	glVertexAttribPointer(0, 2, GL_FLOAT, GL_FALSE, 0, 0);
	glEnableVertexAttribArray(0);
	glViewport(0, 0, W, H);

	static uint8_t pixels[H][W][4];
	int failed = 0;
	uint64_t checksum = 0;
	for (long iteration = 1; iteration <= iterations && !failed; iteration++) {
		memset(pixels, 0, sizeof(pixels));
		glClearColor(0, 0, 1, 1);
		glClear(GL_COLOR_BUFFER_BIT);
		glUseProgram(color_program);
		glUniform4f(glGetUniformLocation(color_program, "color"), 1, 0, 0, 1);
		glDrawArrays(GL_TRIANGLE_STRIP, 0, 4);
		glUseProgram(texture_program);
		glActiveTexture(GL_TEXTURE0);
		glBindTexture(GL_TEXTURE_2D, tex);
		glUniform1i(glGetUniformLocation(texture_program, "tex"), 0);
		glDrawArrays(GL_TRIANGLE_STRIP, 4, 4);
		glReadPixels(0, 0, W, H, GL_RGBA, GL_UNSIGNED_BYTE, pixels);
		if (glGetError() != GL_NO_ERROR) {
			die("drawing");
		}
		// FNV-1a over the pixels, rows from the bottom.
		checksum = 0xcbf29ce484222325ull;
		for (int y = 0; y < H; y++) {
			for (int x = 0; x < W; x++) {
				const uint8_t *want = expected(x, y);
				if (memcmp(pixels[y][x], want, 4) != 0) {
					if (failed++ < 5) {
						fprintf(stderr, "gloffscreen: pixel (%d, %d) is %u,%u,%u,%u, want %u,%u,%u,%u\n",
						        x, y, pixels[y][x][0], pixels[y][x][1], pixels[y][x][2],
						        pixels[y][x][3], want[0], want[1], want[2], want[3]);
					}
				}
				for (int c = 0; c < 4; c++) {
					checksum = (checksum ^ pixels[y][x][c]) * 0x100000001b3ull;
				}
			}
		}
		if (iterations > 1 && !failed) {
			printf("gloffscreen: iteration %ld ok\n", iteration);
			fflush(stdout);
		}
		if (delay_ms > 0 && iteration < iterations) {
			usleep((useconds_t)(delay_ms * 1000));
		}
	}
	if (failed) {
		fprintf(stderr, "gloffscreen: %d pixels differ\n", failed);
		return 1;
	}
	printf("gloffscreen: ok checksum=%016llx\n", (unsigned long long)checksum);
	eglMakeCurrent(dpy, EGL_NO_SURFACE, EGL_NO_SURFACE, EGL_NO_CONTEXT);
	eglDestroyContext(dpy, ctx);
	eglTerminate(dpy);
	return 0;
}
