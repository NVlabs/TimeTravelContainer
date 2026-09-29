/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

#include <errno.h>
#include <fcntl.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>

static void write_marker(const char *path, uint64_t value) {
  FILE *file = fopen(path, "w");
  if (file == NULL) {
    perror(path);
    exit(1);
  }
  fprintf(file, "%llu\n", (unsigned long long)value);
  if (fclose(file) != 0) {
    perror(path);
    exit(1);
  }
}

int main(int argc, char **argv) {
  if (argc != 5) {
    fprintf(stderr, "usage: %s MEMORY_MIB sequential|random TOUCH_FRACTION ROUNDS\n", argv[0]);
    return 2;
  }
  char *end = NULL;
  uint64_t memory_mib = strtoull(argv[1], &end, 10);
  if (*argv[1] == '\0' || *end != '\0' || memory_mib == 0) {
    fprintf(stderr, "invalid memory size\n");
    return 2;
  }
  int random_access = strcmp(argv[2], "random") == 0;
  if (!random_access && strcmp(argv[2], "sequential") != 0) {
    fprintf(stderr, "invalid access pattern\n");
    return 2;
  }
  double fraction = strtod(argv[3], &end);
  if (*argv[3] == '\0' || *end != '\0' || fraction <= 0 || fraction > 1) {
    fprintf(stderr, "invalid touch fraction\n");
    return 2;
  }
  uint64_t rounds = strtoull(argv[4], &end, 10);
  if (*argv[4] == '\0' || *end != '\0' || rounds == 0) {
    fprintf(stderr, "invalid round count\n");
    return 2;
  }
  const size_t page_size = 4096;
  if (memory_mib > SIZE_MAX / (1024 * 1024)) {
    fprintf(stderr, "memory size overflows\n");
    return 2;
  }
  size_t bytes = (size_t)memory_mib * 1024 * 1024;
  size_t pages = bytes / page_size;
  volatile unsigned char *memory = mmap(NULL, bytes, PROT_READ | PROT_WRITE,
                                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (memory == MAP_FAILED) {
    perror("mmap");
    return 1;
  }
  for (size_t page = 0; page < pages; ++page) {
    memory[page * page_size] = (unsigned char)(page * 131 + 17);
  }
  write_marker("/tmp/ttc-bench-ready", (uint64_t)getpid());
  if (raise(SIGSTOP) != 0) {
    perror("raise(SIGSTOP)");
    return 1;
  }

  uint64_t accesses = (uint64_t)((double)pages * fraction);
  if (accesses == 0) {
    accesses = 1;
  }
  uint64_t state = 0x9e3779b97f4a7c15ULL;
  uint64_t sum = 0;
  for (uint64_t round = 0; round < rounds; ++round) {
    for (uint64_t access = 0; access < accesses; ++access) {
      uint64_t page = access % pages;
      if (random_access) {
        state ^= state << 13;
        state ^= state >> 7;
        state ^= state << 17;
        page = state % pages;
      }
      sum += memory[page * page_size];
    }
  }
  write_marker("/tmp/ttc-bench-done", sum);
  for (;;) {
    pause();
  }
}
