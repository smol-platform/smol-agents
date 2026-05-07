# Build CO-RE BPF objects.
#
# Requires: clang, llvm, bpftool (for vmlinux.h generation if not bundled).

CLANG ?= clang
BPFTOOL ?= bpftool

OBJ_DIR ?= ../../bpf/build
TARGETS := syscalls.bpf.o network.bpf.o

CFLAGS := -O2 -g -Wall -Werror -target bpf -D__TARGET_ARCH_x86

.PHONY: all
all: $(addprefix $(OBJ_DIR)/,$(TARGETS))

$(OBJ_DIR):
	mkdir -p $(OBJ_DIR)

$(OBJ_DIR)/vmlinux.h: | $(OBJ_DIR)
	$(BPFTOOL) btf dump file /sys/kernel/btf/vmlinux format c > $@

$(OBJ_DIR)/%.bpf.o: %.bpf.c $(OBJ_DIR)/vmlinux.h
	$(CLANG) $(CFLAGS) -I$(OBJ_DIR) -c $< -o $@

.PHONY: clean
clean:
	rm -rf $(OBJ_DIR)
