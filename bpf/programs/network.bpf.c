// SPDX-License-Identifier: Apache-2.0
// CO-RE BPF program: emit an event on every TCP connect.
//
// Implements partial network observability for R-EBP-1.
//
// Compiled with: clang -O2 -g -target bpf -c network.bpf.c -o network.bpf.o

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "Apache-2.0";

struct connect_event {
    __u64 ts_ns;
    __u32 pid;
    __u32 tgid;
    __u64 cgroup_id;
    __u32 saddr; // IPv4 host byte order
    __u32 daddr;
    __u16 sport;
    __u16 dport;
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 20);
} events SEC(".maps");

SEC("kprobe/tcp_v4_connect")
int BPF_KPROBE(tcp_v4_connect, struct sock *sk) {
    struct connect_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
    if (!e)
        return 0;

    __u64 pt = bpf_get_current_pid_tgid();
    e->pid = (__u32)pt;
    e->tgid = pt >> 32;
    e->ts_ns = bpf_ktime_get_ns();
    e->cgroup_id = bpf_get_current_cgroup_id();

    BPF_CORE_READ_INTO(&e->saddr, sk, __sk_common.skc_rcv_saddr);
    BPF_CORE_READ_INTO(&e->daddr, sk, __sk_common.skc_daddr);
    BPF_CORE_READ_INTO(&e->sport, sk, __sk_common.skc_num);
    BPF_CORE_READ_INTO(&e->dport, sk, __sk_common.skc_dport);

    bpf_ringbuf_submit(e, 0);
    return 0;
}
