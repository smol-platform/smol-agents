// SPDX-License-Identifier: Apache-2.0
// CO-RE BPF program: emit a small event on every sys_enter.
//
// Implements the on-host visibility leg of R-SBX-2 (host-side eBPF
// observes sandboxed agent syscalls).
//
// Compiled with: clang -O2 -g -target bpf -c syscalls.bpf.c -o syscalls.bpf.o

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

/* The Linux BPF verifier rejects calls to GPL-restricted helpers
 * (bpf_probe_read_kernel, bpf_get_current_task, …) unless the program
 * declares a GPL-compatible license. Dual-license THIS kernel-side BPF
 * file — userland in this repo stays Apache-2.0. */
char LICENSE[] SEC("license") = "Dual BSD/GPL";

struct sysenter_event {
    __u64 ts_ns;
    __u32 pid;
    __u32 tgid;
    __u64 cgroup_id;
    __u32 syscall_nr;
    char  comm[16];
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 20); // 1 MiB
} events SEC(".maps");

SEC("raw_tracepoint/sys_enter")
int handle_sys_enter(struct bpf_raw_tracepoint_args *ctx) {
    struct sysenter_event *e;
    e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
    if (!e)
        return 0;

    __u64 pid_tgid = bpf_get_current_pid_tgid();
    e->pid = (__u32)pid_tgid;
    e->tgid = pid_tgid >> 32;
    e->ts_ns = bpf_ktime_get_ns();
    e->cgroup_id = bpf_get_current_cgroup_id();
    // ctx->args[1] is the syscall id on x86_64; portable accessors come via
    // CO-RE in newer kernels.
    e->syscall_nr = (__u32)ctx->args[1];
    bpf_get_current_comm(&e->comm, sizeof(e->comm));

    bpf_ringbuf_submit(e, 0);
    return 0;
}
