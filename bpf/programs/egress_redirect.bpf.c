// SPDX-License-Identifier: Apache-2.0
//
// AgentNetwork host-side egress policy. Two programs in one object:
//
//   redirect_to_sidecar  cgroup/connect4   — rewrites destination to
//                                            the local sidecar when
//                                            the destination matches a
//                                            tenant-supplied LPM trie.
//                                            R-AN-EBPF-1.
//
//   allow_only           cgroup_skb/egress — drops packets whose
//                                            (cgroup, dst, port, proto)
//                                            tuple is not in the
//                                            allow-list. Default deny.
//                                            R-AN-EBPF-2.
//
// Build: clang -O2 -g -target bpf -c egress_redirect.bpf.c -o egress_redirect.bpf.o

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

char LICENSE[] SEC("license") = "Apache-2.0";

// ---------------------------------------------------------------------
// redirect_cidrs — LPM trie keyed by an IPv4 prefix; the value is the
// (sidecar_ip, sidecar_port) the connect should be rewritten to.
// ---------------------------------------------------------------------
struct redirect_key {
    __u32 prefixlen;
    __u32 addr; // network byte order
};

struct redirect_val {
    __u32 sidecar_ip;
    __u16 sidecar_port;
    __u16 _pad;
};

struct {
    __uint(type, BPF_MAP_TYPE_LPM_TRIE);
    __type(key, struct redirect_key);
    __type(value, struct redirect_val);
    __uint(max_entries, 1024);
    __uint(map_flags, BPF_F_NO_PREALLOC);
} redirect_cidrs SEC(".maps");

SEC("cgroup/connect4")
int redirect_to_sidecar(struct bpf_sock_addr *ctx) {
    if (ctx->user_family != AF_INET)
        return 1;
    if (ctx->protocol != IPPROTO_TCP)
        return 1;

    struct redirect_key k = {
        .prefixlen = 32,
        .addr      = ctx->user_ip4, // already in network byte order
    };
    struct redirect_val *v = bpf_map_lookup_elem(&redirect_cidrs, &k);
    if (!v)
        return 1;

    ctx->user_ip4   = v->sidecar_ip;
    ctx->user_port  = bpf_htons(v->sidecar_port);
    return 1;
}

// ---------------------------------------------------------------------
// allow_list — hash keyed by (cgroup_id, dst_ip, dst_port, proto).
// Membership = allowed; absence = drop.
// ---------------------------------------------------------------------
struct allow_key {
    __u64 cgroup_id;
    __u32 dst_ip;   // network byte order
    __u16 dst_port; // host byte order in the hash to avoid endian skew
    __u8  proto;    // 6=TCP, 17=UDP
    __u8  _pad;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __type(key, struct allow_key);
    __type(value, __u8);
    __uint(max_entries, 65536);
} allow_list SEC(".maps");

// audit ringbuf: emit (cgroup_id, dst_ip, dst_port, allowed) per
// outbound TCP connect attempt. Userspace correlates cgroup → SPIFFE.
struct audit_event {
    __u64 cgroup_id;
    __u32 dst_ip;
    __u16 dst_port;
    __u8  proto;
    __u8  outcome; // 0=drop, 1=allow
};

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 18);
} egress_audit SEC(".maps");

static __always_inline int parse_l3l4(struct __sk_buff *skb, __u32 *dst_ip, __u16 *dst_port, __u8 *proto) {
    void *data     = (void *)(long)skb->data;
    void *data_end = (void *)(long)skb->data_end;
    struct iphdr *iph = data;
    if ((void *)(iph + 1) > data_end)
        return 0;
    if (iph->version != 4)
        return 0;

    *dst_ip = iph->daddr;
    *proto  = iph->protocol;

    if (iph->protocol == IPPROTO_TCP) {
        struct tcphdr *th = (void *)iph + iph->ihl * 4;
        if ((void *)(th + 1) > data_end)
            return 0;
        *dst_port = bpf_ntohs(th->dest);
        return 1;
    }
    if (iph->protocol == IPPROTO_UDP) {
        struct udphdr *uh = (void *)iph + iph->ihl * 4;
        if ((void *)(uh + 1) > data_end)
            return 0;
        *dst_port = bpf_ntohs(uh->dest);
        return 1;
    }
    return 0;
}

SEC("cgroup_skb/egress")
int allow_only(struct __sk_buff *skb) {
    __u32 dst_ip = 0;
    __u16 dst_port = 0;
    __u8 proto = 0;
    if (!parse_l3l4(skb, &dst_ip, &dst_port, &proto))
        return 1; // not L3/L4 we understand → pass

    struct allow_key k = {
        .cgroup_id = bpf_get_current_cgroup_id(),
        .dst_ip    = dst_ip,
        .dst_port  = dst_port,
        .proto     = proto,
    };
    __u8 *allowed = bpf_map_lookup_elem(&allow_list, &k);
    __u8 outcome = (allowed && *allowed) ? 1 : 0;

    // Emit audit event (best-effort; full ringbuf is fine to drop).
    struct audit_event *e = bpf_ringbuf_reserve(&egress_audit, sizeof(*e), 0);
    if (e) {
        e->cgroup_id = k.cgroup_id;
        e->dst_ip    = dst_ip;
        e->dst_port  = dst_port;
        e->proto     = proto;
        e->outcome   = outcome;
        bpf_ringbuf_submit(e, 0);
    }

    return outcome; // 1=pass, 0=drop
}
