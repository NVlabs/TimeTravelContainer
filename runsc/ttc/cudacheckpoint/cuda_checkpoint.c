/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

/*
 * ttc-cuda-checkpoint does what gVisor's nvproxy asks of NVIDIA's
 * cuda-checkpoint (pkg/sentry/control/state_cuda.go), which TTC runs in the
 * containers of --nvproxy-fork=snapshot families when they fork:
 *
 *   ttc-cuda-checkpoint --get-state --pid PID
 *   ttc-cuda-checkpoint --toggle --pid PID
 *   ttc-cuda-checkpoint --action lock|checkpoint|restore|unlock --pid PID
 *
 * --toggle checkpoints a running process, which then holds its GPU state in
 * its own memory, and restores a checkpointed one. It calls CUDA's
 * checkpoint API, in the driver of CUDA 12.8 and later, which it loads from
 * the container's libcuda.so.1, the host's driver.
 */

#include <dlfcn.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef int CUresult;

/* CUprocessState. */
enum {
	CU_PROCESS_STATE_RUNNING = 0,
	CU_PROCESS_STATE_LOCKED = 1,
	CU_PROCESS_STATE_CHECKPOINTED = 2,
	CU_PROCESS_STATE_FAILED = 3,
};

/*
 * The arguments of the checkpoint calls, CUcheckpointLockArgs and the
 * others, are 64 bytes each, which zeroes leave at their defaults. The first
 * 32 bits of the lock's are its timeout in milliseconds.
 */
struct args {
	unsigned int first;
	unsigned char rest[60];
};

typedef CUresult (*process_call)(int pid, struct args *args);

static CUresult (*cu_init)(unsigned int flags);
static CUresult (*cu_get_error_name)(CUresult error, const char **name);
static CUresult (*cu_get_state)(int pid, int *state);
static process_call cu_lock, cu_checkpoint, cu_restore, cu_unlock;

static const char *error_name(CUresult error) {
	const char *name = NULL;
	if (cu_get_error_name == NULL || cu_get_error_name(error, &name) != 0 ||
	    name == NULL) {
		return "unknown error";
	}
	return name;
}

static const char *state_name(int state) {
	switch (state) {
	case CU_PROCESS_STATE_RUNNING:
		return "running";
	case CU_PROCESS_STATE_LOCKED:
		return "locked";
	case CU_PROCESS_STATE_CHECKPOINTED:
		return "checkpointed";
	case CU_PROCESS_STATE_FAILED:
		return "failed";
	default:
		return "unknown";
	}
}

static void *symbol(void *libcuda, const char *name) {
	void *f = dlsym(libcuda, name);
	if (f == NULL) {
		fprintf(stderr, "ttc-cuda-checkpoint: the CUDA driver has no %s: it "
				"needs a driver of CUDA 12.8 or later\n",
			name);
		exit(1);
	}
	return f;
}

static void load_driver(void) {
	void *libcuda = dlopen("libcuda.so.1", RTLD_NOW | RTLD_LOCAL);
	if (libcuda == NULL) {
		fprintf(stderr, "ttc-cuda-checkpoint: loading the CUDA driver: %s\n",
			dlerror());
		exit(1);
	}
	cu_init = symbol(libcuda, "cuInit");
	cu_get_error_name = symbol(libcuda, "cuGetErrorName");
	cu_get_state = symbol(libcuda, "cuCheckpointProcessGetState");
	cu_lock = symbol(libcuda, "cuCheckpointProcessLock");
	cu_checkpoint = symbol(libcuda, "cuCheckpointProcessCheckpoint");
	cu_restore = symbol(libcuda, "cuCheckpointProcessRestore");
	cu_unlock = symbol(libcuda, "cuCheckpointProcessUnlock");
}

static int get_state(int pid, int *state) {
	CUresult err = cu_get_state(pid, state);
	if (err != 0) {
		fprintf(stderr, "ttc-cuda-checkpoint: the state of process %d: %s\n",
			pid, error_name(err));
		return -1;
	}
	return 0;
}

/* call runs one step of a checkpoint or a restore of process pid. */
static int call(const char *what, process_call f, int pid,
		unsigned int timeout_ms) {
	struct args args;
	memset(&args, 0, sizeof(args));
	args.first = timeout_ms;
	CUresult err = f(pid, &args);
	if (err != 0) {
		fprintf(stderr, "ttc-cuda-checkpoint: %s of process %d: %s\n", what,
			pid, error_name(err));
		return -1;
	}
	return 0;
}

static int restore(int pid) {
	/* A restore needs CUDA initialized in this process, unless the GPUs are
	 * in persistence mode. */
	CUresult err = cu_init(0);
	if (err != 0) {
		fprintf(stderr, "ttc-cuda-checkpoint: initializing CUDA: %s\n",
			error_name(err));
		return -1;
	}
	return call("restore", cu_restore, pid, 0);
}

static int toggle(int pid, unsigned int timeout_ms) {
	int state;
	if (get_state(pid, &state) < 0) {
		return -1;
	}
	switch (state) {
	case CU_PROCESS_STATE_RUNNING:
		if (call("lock", cu_lock, pid, timeout_ms) < 0) {
			return -1;
		}
		if (call("checkpoint", cu_checkpoint, pid, 0) < 0) {
			/* Leave the process as it was. */
			call("unlock", cu_unlock, pid, 0);
			return -1;
		}
		return 0;
	case CU_PROCESS_STATE_CHECKPOINTED:
		if (restore(pid) < 0) {
			return -1;
		}
		return call("unlock", cu_unlock, pid, 0);
	default:
		fprintf(stderr, "ttc-cuda-checkpoint: process %d is %s, neither "
				"running nor checkpointed\n",
			pid, state_name(state));
		return -1;
	}
}

static void usage(void) {
	fprintf(stderr,
		"usage: ttc-cuda-checkpoint --get-state --pid PID\n"
		"       ttc-cuda-checkpoint --toggle [--timeout MS] --pid PID\n"
		"       ttc-cuda-checkpoint --action lock|checkpoint|restore|unlock "
		"[--timeout MS] --pid PID\n");
	exit(2);
}

static long parse_number(const char *s) {
	char *end;
	errno = 0;
	long n = strtol(s, &end, 10);
	if (errno != 0 || end == s || *end != '\0' || n < 0 || n > 0x7fffffff) {
		usage();
	}
	return n;
}

int main(int argc, char **argv) {
	const char *op = NULL;
	const char *action = NULL;
	long pid = -1;
	unsigned int timeout_ms = 0;
	for (int i = 1; i < argc; i++) {
		const char *arg = argv[i];
		if (strcmp(arg, "--get-state") == 0 || strcmp(arg, "--toggle") == 0) {
			op = arg;
		} else if (strcmp(arg, "--action") == 0 && i + 1 < argc) {
			op = arg;
			action = argv[++i];
		} else if ((strcmp(arg, "--pid") == 0 || strcmp(arg, "-p") == 0) &&
			   i + 1 < argc) {
			pid = parse_number(argv[++i]);
		} else if (strcmp(arg, "--timeout") == 0 && i + 1 < argc) {
			timeout_ms = (unsigned int)parse_number(argv[++i]);
		} else {
			usage();
		}
	}
	if (op == NULL || pid <= 0) {
		usage();
	}
	load_driver();

	int status;
	if (strcmp(op, "--get-state") == 0) {
		int state;
		status = get_state((int)pid, &state);
		if (status == 0) {
			printf("%s\n", state_name(state));
		}
	} else if (strcmp(op, "--toggle") == 0) {
		status = toggle((int)pid, timeout_ms);
	} else if (strcmp(action, "lock") == 0) {
		status = call("lock", cu_lock, (int)pid, timeout_ms);
	} else if (strcmp(action, "checkpoint") == 0) {
		status = call("checkpoint", cu_checkpoint, (int)pid, 0);
	} else if (strcmp(action, "restore") == 0) {
		status = restore((int)pid);
	} else if (strcmp(action, "unlock") == 0) {
		status = call("unlock", cu_unlock, (int)pid, 0);
	} else {
		usage();
		status = -1;
	}
	return status == 0 ? 0 : 1;
}
