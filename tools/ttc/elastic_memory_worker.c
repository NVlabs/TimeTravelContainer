/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// elastic_memory_worker is a small live-fork qualification workload. It keeps
// anonymous memory mapped and resident, then accepts branch-local write
// commands through /tmp/ttc-write after SIGCONT. Command format:
//
//   OFFSET_MIB LENGTH_MIB BYTE_VALUE
//
// The worker writes one byte in every affected base page, publishes
// /tmp/ttc-write-done, and stops again. Since /tmp and process memory are both
// forked by TTC, siblings can issue independent mutations to the same original
// memory image.

#include <errno.h>
#include <signal.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <unistd.h>

static void marker(const char *path) {
  FILE *file = fopen(path, "w");
  if (file == NULL) {
    perror(path);
    exit(1);
  }
  if (fclose(file) != 0) {
    perror(path);
    exit(1);
  }
}

int main(int argc, char **argv) {
  if (argc != 2) {
    fprintf(stderr, "usage: %s MEMORY_MIB\n", argv[0]);
    return 2;
  }
  char *end = NULL;
  uint64_t memory_mib = strtoull(argv[1], &end, 10);
  if (*argv[1] == '\0' || *end != '\0' || memory_mib == 0 ||
      memory_mib > SIZE_MAX / (1024 * 1024)) {
    fprintf(stderr, "invalid memory size\n");
    return 2;
  }
  const size_t page_size = 4096;
  size_t bytes = (size_t)memory_mib * 1024 * 1024;
  volatile unsigned char *memory = mmap(NULL, bytes, PROT_READ | PROT_WRITE,
                                        MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (memory == MAP_FAILED) {
    perror("mmap");
    return 1;
  }
  for (size_t offset = 0; offset < bytes; offset += page_size) {
    memory[offset] = (unsigned char)(offset / page_size);
  }
  marker("/tmp/ttc-ready");

  for (;;) {
    if (raise(SIGSTOP) != 0) {
      perror("raise(SIGSTOP)");
      return 1;
    }
    FILE *command = fopen("/tmp/ttc-write", "r");
    if (command == NULL) {
      if (errno == ENOENT) {
        continue;
      }
      perror("/tmp/ttc-write");
      return 1;
    }
    unsigned long long parsed_offset, parsed_length, parsed_value;
    if (fscanf(command, "%llu %llu %llu", &parsed_offset, &parsed_length,
               &parsed_value) != 3 ||
        fclose(command) != 0) {
      fprintf(stderr, "invalid /tmp/ttc-write\n");
      return 1;
    }
    uint64_t offset_mib = parsed_offset;
    uint64_t length_mib = parsed_length;
    uint64_t value = parsed_value;
    if (offset_mib > memory_mib || length_mib > memory_mib - offset_mib ||
        value > 255) {
      fprintf(stderr, "out-of-range /tmp/ttc-write\n");
      return 1;
    }
    size_t start = (size_t)offset_mib * 1024 * 1024;
    size_t end_offset = start + (size_t)length_mib * 1024 * 1024;
    for (size_t offset = start; offset < end_offset; offset += page_size) {
      memory[offset] = (unsigned char)value;
    }
    if (unlink("/tmp/ttc-write") != 0) {
      perror("unlink /tmp/ttc-write");
      return 1;
    }
    marker("/tmp/ttc-write-done");
  }
}
