// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cudaloop counts on the GPU: each step, a kernel adds 1 to every element of
// a 64 MiB buffer in device memory, and the host checks elements across it.
// It prints "cudaloop: step N ok" each step, and "cudaloop: done" after
// -n STEPS steps; -i MS sleeps between steps. A CUDA error ends it with
// "cudaloop: error ..." and status 3, a wrong count with status 4.
//
// A fork with --nvproxy-fork=snapshot copies the count: both branches count
// on from it. With --nvproxy-fork=device-lost, the child's copy must end,
// not hang or count wrongly.

#include <cuda_runtime.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define CHECK(call)                                                           \
  do {                                                                        \
    cudaError_t err = (call);                                                 \
    if (err != cudaSuccess) {                                                 \
      printf("cudaloop: error %s: %s, at line %d\n", cudaGetErrorName(err),  \
             cudaGetErrorString(err), __LINE__);                              \
      fflush(stdout);                                                         \
      exit(3);                                                                \
    }                                                                         \
  } while (0)

__global__ void add1(unsigned int *buf, size_t n) {
  for (size_t i = blockIdx.x * (size_t)blockDim.x + threadIdx.x; i < n;
       i += (size_t)gridDim.x * blockDim.x) {
    buf[i] += 1;
  }
}

int main(int argc, char **argv) {
  long steps = -1;
  long interval_ms = 100;
  for (int i = 1; i < argc; i++) {
    if (strcmp(argv[i], "-n") == 0 && i + 1 < argc) {
      steps = atol(argv[++i]);
    } else if (strcmp(argv[i], "-i") == 0 && i + 1 < argc) {
      interval_ms = atol(argv[++i]);
    } else {
      fprintf(stderr, "usage: cudaloop [-n STEPS] [-i MS]\n");
      return 2;
    }
  }
  int device;
  cudaDeviceProp prop;
  CHECK(cudaGetDevice(&device));
  CHECK(cudaGetDeviceProperties(&prop, device));
  printf("cudaloop: device %s, pid %d\n", prop.name, (int)getpid());
  fflush(stdout);

  const size_t n = 16 << 20;
  unsigned int *buf;
  CHECK(cudaMalloc(&buf, n * sizeof(*buf)));
  CHECK(cudaMemset(buf, 0, n * sizeof(*buf)));
  const size_t checked[] = {0, n / 3, n / 2, 2 * n / 3, n - 1};
  for (long step = 1; steps < 0 || step <= steps; step++) {
    add1<<<1024, 256>>>(buf, n);
    CHECK(cudaGetLastError());
    CHECK(cudaDeviceSynchronize());
    for (size_t k = 0; k < sizeof(checked) / sizeof(checked[0]); k++) {
      unsigned int value;
      CHECK(cudaMemcpy(&value, buf + checked[k], sizeof(value),
                       cudaMemcpyDeviceToHost));
      if (value != (unsigned int)step) {
        printf("cudaloop: step %ld: element %zu is %u\n", step, checked[k],
               value);
        fflush(stdout);
        return 4;
      }
    }
    printf("cudaloop: step %ld ok\n", step);
    fflush(stdout);
    if (interval_ms > 0) {
      usleep(interval_ms * 1000);
    }
  }
  CHECK(cudaFree(buf));
  printf("cudaloop: done\n");
  return 0;
}
