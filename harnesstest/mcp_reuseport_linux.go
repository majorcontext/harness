//go:build linux && !mips && !mipsle && !mips64 && !mips64le

package harnesstest

// The frozen syscall package omits SO_REUSEPORT on x86.
const mcpReusePortOption = 0xf
