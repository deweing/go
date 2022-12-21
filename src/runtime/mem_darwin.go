// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"unsafe"
)

// Don't split the stack as this function may be invoked without a valid G,
// which prevents us from allocating more stack.
//
//go:nosplit
func sysAllocOS(n uintptr) unsafe.Pointer {
	v, err := mmap(nil, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		return nil
	}
	return v
}

func sysUnusedOS(v unsafe.Pointer, n uintptr) {
	// MADV_FREE_REUSABLE is like MADV_FREE except it also propagates
	// accounting information about the process to task_info.
	madvise(v, n, _MADV_FREE_REUSABLE)
}

func sysUsedOS(v unsafe.Pointer, n uintptr) {
	// MADV_FREE_REUSE is necessary to keep the kernel's accounting
	// accurate. If called on any memory region that hasn't been
	// MADV_FREE_REUSABLE'd, it's a no-op.
	madvise(v, n, _MADV_FREE_REUSE)
}

func sysHugePageOS(v unsafe.Pointer, n uintptr) {
}

// Don't split the stack as this function may be invoked without a valid G,
// which prevents us from allocating more stack.
//
//go:nosplit
func sysFreeOS(v unsafe.Pointer, n uintptr) {
	munmap(v, n)
}

func sysFaultOS(v unsafe.Pointer, n uintptr) {
	mmap(v, n, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE|_MAP_FIXED, -1, 0)
}

// 预留内存地址
// 该函数会执行 mmap 系统调用，使用 PORT_NONE，并选择匿名、私有映射（Anonymous/private mapping）的方式 _MAP_ANON
// 向操作系统申请预留虚拟地址空间，调用结束后预留地址所在的内存处于预留态，
// 处于该状态的内存只用于提前想操作系统预告其可能的使用状态，不能对得到的内存进行安全的读写操作
func sysReserveOS(v unsafe.Pointer, n uintptr) unsafe.Pointer {
	p, err := mmap(v, n, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		return nil
	}
	return p
}

const _ENOMEM = 12

// 该函数执行mmap系统调用，将保留好的内存进一步映射，将其转换为 PORT_READ 和 PORT_WRITE 可读写状态
func sysMapOS(v unsafe.Pointer, n uintptr) {
	p, err := mmap(v, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_FIXED|_MAP_PRIVATE, -1, 0)
	if err == _ENOMEM {
		throw("runtime: out of memory")
	}
	if p != v || err != 0 {
		print("runtime: mmap(", v, ", ", n, ") returned ", p, ", ", err, "\n")
		throw("runtime: cannot map pages in arena address space")
	}
}
