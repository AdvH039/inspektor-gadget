/* SPDX-License-Identifier: (LGPL-2.1 OR BSD-2-Clause) */
/* Copyright (c) 2021 Hengqi Chen */
#include <vmlinux.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include "filetop.h"
#include "stat.h"
#include <gadget/mntns.h>
#include <gadget/mntns_filter.h>

#define MAX_ENTRIES 10240

const volatile pid_t target_pid = 0;
const volatile bool regular_file_only = true;
static struct file_stat zero_value = {};

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, MAX_ENTRIES);
	__type(key, struct file_id);
	__type(value, struct file_stat);
} entries SEC(".maps");

static void get_file_path(struct file *file, __u8 *buf, size_t size)
{
	struct qstr dname;

	dname = BPF_CORE_READ(file, f_path.dentry, d_name);
	bpf_probe_read_kernel(buf, size, dname.name);
}

static int probe_entry(struct pt_regs *ctx, struct file *file, size_t count,
		       enum op op)
{
	__u64 pid_tgid = bpf_get_current_pid_tgid();
	__u32 pid = pid_tgid >> 32;
	__u32 tid = (__u32)pid_tgid;
	int mode;
	struct file_id key = {};
	struct file_stat *valuep;
	u64 mntns_id;

	if (target_pid && target_pid != pid)
		return 0;

	mntns_id = gadget_get_current_mntns_id();

	if (gadget_should_discard_mntns_id(mntns_id))
		return 0;

	mode = BPF_CORE_READ(file, f_inode, i_mode);
	if (regular_file_only && !S_ISREG(mode))
		return 0;

	key.dev = BPF_CORE_READ(file, f_inode, i_rdev);
	key.inode = BPF_CORE_READ(file, f_inode, i_ino);
	key.pid = pid;
	key.tid = tid;
	valuep = bpf_map_lookup_elem(&entries, &key);
	if (!valuep) {
		bpf_map_update_elem(&entries, &key, &zero_value, BPF_ANY);
		valuep = bpf_map_lookup_elem(&entries, &key);
		if (!valuep)
			return 0;
		valuep->pid = pid;
		valuep->tid = tid;
		valuep->mntns_id = mntns_id;
		bpf_get_current_comm(&valuep->comm, sizeof(valuep->comm));
		get_file_path(file, valuep->filename, sizeof(valuep->filename));
		if (S_ISREG(mode)) {
			valuep->type_ = 'R';
		} else if (S_ISSOCK(mode)) {
			valuep->type_ = 'S';
		} else {
			valuep->type_ = 'O';
		}
	}
	if (op == READ) {
		valuep->reads++;
		valuep->read_bytes += count;
	} else { /* op == WRITE */
		valuep->writes++;
		valuep->write_bytes += count;
	}
	return 0;
};

SEC("kprobe/vfs_read")
int BPF_KPROBE(ig_topfile_rd_e, struct file *file, char *buf, size_t count,
	       loff_t *pos)
{
	return probe_entry(ctx, file, count, READ);
}

SEC("kprobe/vfs_write")
int BPF_KPROBE(ig_topfile_wr_e, struct file *file, const char *buf,
	       size_t count, loff_t *pos)
{
	return probe_entry(ctx, file, count, WRITE);
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
