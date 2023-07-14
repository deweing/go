// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Memory statistics

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

type mstats struct {
	// Statistics about malloc heap.
	heapStats consistentHeapStats

	// Statistics about stacks.
	stacks_sys sysMemStat // only counts newosproc0 stack in mstats; differs from MemStats.StackSys

	// Statistics about allocation of low-level fixed-size structures.
	mspan_sys    sysMemStat
	mcache_sys   sysMemStat
	buckhash_sys sysMemStat // profiling bucket hash table

	// Statistics about GC overhead.
	gcMiscSys sysMemStat // updated atomically or during STW

	// Miscellaneous statistics.
	other_sys sysMemStat // updated atomically or during STW

	// Statistics about the garbage collector.

	// Protected by mheap or stopping the world during GC.
	last_gc_unix    uint64 // last gc (in unix time)
	pause_total_ns  uint64
	pause_ns        [256]uint64 // circular buffer of recent gc pause lengths
	pause_end       [256]uint64 // circular buffer of recent gc end times (nanoseconds since 1970)
	numgc           uint32
	numforcedgc     uint32  // number of user-forced GCs
	gc_cpu_fraction float64 // fraction of CPU time used by GC

	last_gc_nanotime uint64 // last gc (monotonic time)
	lastHeapInUse    uint64 // heapInUse at mark termination of the previous GC

	enablegc bool

	// gcPauseDist represents the distribution of all GC-related
	// application pauses in the runtime.
	//
	// Each individual pause is counted separately, unlike pause_ns.
	gcPauseDist timeHistogram
}

var memstats mstats

// A MemStats records statistics about the memory allocator.
type MemStats struct {
	// General statistics.

	// Alloc is bytes of allocated heap objects.
	//
	// This is the same as HeapAlloc (see below).
	//
	// 已分配的对象的字节数
	// 这个和HeapAlloc是一样的
	Alloc uint64

	// TotalAlloc is cumulative bytes allocated for heap objects.
	//
	// TotalAlloc increases as heap objects are allocated, but
	// unlike Alloc and HeapAlloc, it does not decrease when
	// objects are freed.
	//
	// 分配的字节数累计之和
	// TotalAlloc会随着分配的对象增加而增加，但是不会随着对象的释放而减少
	TotalAlloc uint64

	// Sys is the total bytes of memory obtained from the OS.
	//
	// Sys is the sum of the XSys fields below. Sys measures the
	// virtual address space reserved by the Go runtime for the
	// heap, stacks, and other internal data structures. It's
	// likely that not all of the virtual address space is backed
	// by physical memory at any given moment, though in general
	// it all was at some point.
	//
	// 从操作系统中获取的内存总数
	// Sys是下面XSys字段的值的总和，是为堆、栈和其他内部数据结构保留的虚拟地址空间
	// 注意虚拟内存空间和物理内存的区别
	Sys uint64

	// Lookups is the number of pointer lookups performed by the
	// runtime.
	//
	// This is primarily useful for debugging runtime internals.
	//
	// 运行时地址查找的次数，主要用在运行时内部调试上
	Lookups uint64

	// Mallocs is the cumulative count of heap objects allocated.
	// The number of live objects is Mallocs - Frees.
	//
	// 堆对象分配的累计次数
	// 活跃对象的数量是Mallocs - Frees
	Mallocs uint64

	// Frees is the cumulative count of heap objects freed.
	//
	// 释放的对象数
	Frees uint64

	// Heap memory statistics.
	//
	// Interpreting the heap statistics requires some knowledge of
	// how Go organizes memory. Go divides the virtual address
	// space of the heap into "spans", which are contiguous
	// regions of memory 8K or larger. A span may be in one of
	// three states:
	//
	// An "idle" span contains no objects or other data. The
	// physical memory backing an idle span can be released back
	// to the OS (but the virtual address space never is), or it
	// can be converted into an "in use" or "stack" span.
	//
	// An "in use" span contains at least one heap object and may
	// have free space available to allocate more heap objects.
	//
	// A "stack" span is used for goroutine stacks. Stack spans
	// are not considered part of the heap. A span can change
	// between heap and stack memory; it is never used for both
	// simultaneously.

	// HeapAlloc is bytes of allocated heap objects.
	//
	// "Allocated" heap objects include all reachable objects, as
	// well as unreachable objects that the garbage collector has
	// not yet freed. Specifically, HeapAlloc increases as heap
	// objects are allocated and decreases as the heap is swept
	// and unreachable objects are freed. Sweeping occurs
	// incrementally between GC cycles, so these two processes
	// occur simultaneously, and as a result HeapAlloc tends to
	// change smoothly (in contrast with the sawtooth that is
	// typical of stop-the-world garbage collectors).
	//
	// 分配的堆对象的字节数
	//
	// 包括所有可访问的对象以及还未被垃圾回收的不可访问的对象.
	// 所以这个值是变化的，分配对象时会增加，垃圾回收对象时会减少.
	HeapAlloc uint64

	// HeapSys is bytes of heap memory obtained from the OS.
	//
	// HeapSys measures the amount of virtual address space
	// reserved for the heap. This includes virtual address space
	// that has been reserved but not yet used, which consumes no
	// physical memory, but tends to be small, as well as virtual
	// address space for which the physical memory has been
	// returned to the OS after it became unused (see HeapReleased
	// for a measure of the latter).
	//
	// HeapSys estimates the largest size the heap has had.
	//
	// 从操作系统中获取的堆内存大小
	//
	// 虚拟内存空间为堆保留的大小，包括还没有被使用的.
	// HeapSys 可被估算为堆已有的最大尺寸.
	HeapSys uint64

	// HeapIdle is bytes in idle (unused) spans.
	//
	// Idle spans have no objects in them. These spans could be
	// (and may already have been) returned to the OS, or they can
	// be reused for heap allocations, or they can be reused as
	// stack memory.
	//
	// HeapIdle minus HeapReleased estimates the amount of memory
	// that could be returned to the OS, but is being retained by
	// the runtime so it can grow the heap without requesting more
	// memory from the OS. If this difference is significantly
	// larger than the heap size, it indicates there was a recent
	// transient spike in live heap size.
	//
	// HeapIdle是idle(未被使用的)span中的字节数
	//
	// idle span 是指没有任何对象的span，这些span可以返还给操作系统，或者它们可以被重用
	// 或者它们可以用作栈内存
	//
	// HeapIdle减去HeapReleased的值可以当作“可以返回到操作系统但有运行时保留的内存量”。
	// 以便在不向操作系统请求更多内存的情况下增加堆，也就是运行时的“小金库”
	//
	// 如果这个差值明显比堆的大小大很多，说明最近在活动堆上有一次尖峰
	HeapIdle uint64

	// HeapInuse is bytes in in-use spans.
	//
	// In-use spans have at least one object in them. These spans
	// can only be used for other objects of roughly the same
	// size.
	//
	// HeapInuse minus HeapAlloc estimates the amount of memory
	// that has been dedicated to particular size classes, but is
	// not currently being used. This is an upper bound on
	// fragmentation, but in general this memory can be reused
	// efficiently.
	//
	// 正在使用的span的字节大小
	//
	// 正在使用的span时指它至少包含一个对象在其中
	// HeapInuse减去HeapAlloc的值可以当作“已经分配给特定大小类的内存量，但目前没有使用”
	HeapInuse uint64

	// HeapReleased is bytes of physical memory returned to the OS.
	//
	// This counts heap memory from idle spans that was returned
	// to the OS and has not yet been reacquired for the heap.
	//
	// HeapReleased是返回给操作系统的物理内存的字节数
	//
	// 它统计了从idle span中返还给操作系统，没有被重新获取的内存大小.
	HeapReleased uint64

	// HeapObjects is the number of allocated heap objects.
	//
	// Like HeapAlloc, this increases as objects are allocated and
	// decreases as the heap is swept and unreachable objects are
	// freed.
	//
	// HeapObjects 实时统计的分配的堆对象的数量,类似HeapAlloc.
	HeapObjects uint64

	// Stack memory statistics.
	//
	// Stacks are not considered part of the heap, but the runtime
	// can reuse a span of heap memory for stack memory, and
	// vice-versa.

	// StackInuse is bytes in stack spans.
	//
	// In-use stack spans have at least one stack in them. These
	// spans can only be used for other stacks of the same size.
	//
	// There is no StackIdle because unused stack spans are
	// returned to the heap (and hence counted toward HeapIdle).
	//
	// 栈span使用的字节数。
	// 正在使用的栈span是指至少有一个栈在其中.
	//
	//  注意并没有idle的栈span,因为未使用的栈span会被返还给堆(HeapIdle).
	StackInuse uint64

	// StackSys is bytes of stack memory obtained from the OS.
	//
	// StackSys is StackInuse, plus any memory obtained directly
	// from the OS for OS thread stacks (which should be minimal).
	//
	// 从操作系统取得的栈内存大小.
	// 等于StackInuse 再加上为操作系统线程栈获得的内存.
	StackSys uint64

	// Off-heap memory statistics.
	//
	// The following statistics measure runtime-internal
	// structures that are not allocated from heap memory (usually
	// because they are part of implementing the heap). Unlike
	// heap or stack memory, any memory allocated to these
	// structures is dedicated to these structures.
	//
	// These are primarily useful for debugging runtime memory
	// overheads.

	// MSpanInuse is bytes of allocated mspan structures.
	//
	// 分配的mspan数据结构的字节数.
	MSpanInuse uint64

	// MSpanSys is bytes of memory obtained from the OS for mspan
	// structures.
	//
	// 从操作系统为mspan获取的内存字节数.
	MSpanSys uint64

	// MCacheInuse is bytes of allocated mcache structures.
	//
	// 分配的mcache数据结构的字节数.
	MCacheInuse uint64

	// MCacheSys is bytes of memory obtained from the OS for
	// mcache structures.
	//
	// 从操作系统为mcache获取的内存字节数.
	MCacheSys uint64

	// BuckHashSys is bytes of memory in profiling bucket hash tables.
	//
	// 在profiling bucket hash tables中的内存字节数.
	BuckHashSys uint64

	// GCSys is bytes of memory in garbage collection metadata.
	//
	// 垃圾回收元数据使用的内存字节数.
	GCSys uint64

	// OtherSys is bytes of memory in miscellaneous off-heap
	// runtime allocations.
	//
	// off-heap的杂项内存字节数.
	OtherSys uint64

	// Garbage collector statistics.

	// NextGC is the target heap size of the next GC cycle.
	//
	// The garbage collector's goal is to keep HeapAlloc ≤ NextGC.
	// At the end of each GC cycle, the target for the next cycle
	// is computed based on the amount of reachable data and the
	// value of GOGC.
	//
	// 下一次垃圾回收的目标大小，保证 HeapAlloc ≤ NextGC.
	// 基于当前可访问的数据和GOGC的值计算而得.
	NextGC uint64

	// LastGC is the time the last garbage collection finished, as
	// nanoseconds since 1970 (the UNIX epoch).
	//
	// 上一次垃圾回收的时间.
	LastGC uint64

	// PauseTotalNs is the cumulative nanoseconds in GC
	// stop-the-world pauses since the program started.
	//
	// During a stop-the-world pause, all goroutines are paused
	// and only the garbage collector can run.
	//
	// 自程序开始 STW 暂停的累积纳秒数.
	// STW的时候除了垃圾回收器之外所有的goroutine都会暂停.
	PauseTotalNs uint64

	// PauseNs is a circular buffer of recent GC stop-the-world
	// pause times in nanoseconds.
	//
	// The most recent pause is at PauseNs[(NumGC+255)%256]. In
	// general, PauseNs[N%256] records the time paused in the most
	// recent N%256th GC cycle. There may be multiple pauses per
	// GC cycle; this is the sum of all pauses during a cycle.
	//
	//  一个循环buffer，用来记录最近的256个GC STW的暂停时间.
	PauseNs [256]uint64

	// PauseEnd is a circular buffer of recent GC pause end times,
	// as nanoseconds since 1970 (the UNIX epoch).
	//
	// This buffer is filled the same way as PauseNs. There may be
	// multiple pauses per GC cycle; this records the end of the
	// last pause in a cycle.
	//
	// 最近256个GC暂停截止的时间.
	PauseEnd [256]uint64

	// NumGC is the number of completed GC cycles.
	//
	// GC的总次数.
	NumGC uint32

	// NumForcedGC is the number of GC cycles that were forced by
	// the application calling the GC function.
	//
	// 强制GC的次数.
	NumForcedGC uint32

	// GCCPUFraction is the fraction of this program's available
	// CPU time used by the GC since the program started.
	//
	// GCCPUFraction is expressed as a number between 0 and 1,
	// where 0 means GC has consumed none of this program's CPU. A
	// program's available CPU time is defined as the integral of
	// GOMAXPROCS since the program started. That is, if
	// GOMAXPROCS is 2 and a program has been running for 10
	// seconds, its "available CPU" is 20 seconds. GCCPUFraction
	// does not include CPU time used for write barrier activity.
	//
	// This is the same as the fraction of CPU reported by
	// GODEBUG=gctrace=1.
	//
	// 自程序启动后由GC占用的CPU可用时间，数值在 0 到 1 之间.
	// 0代表GC没有消耗程序的CPU. GOMAXPROCS * 程序运行时间等于程序的CPU可用时间.
	GCCPUFraction float64

	// EnableGC indicates that GC is enabled. It is always true,
	// even if GOGC=off.
	//
	// 是否允许GC.
	EnableGC bool

	// DebugGC is currently unused.
	DebugGC bool

	// BySize reports per-size class allocation statistics.
	//
	// BySize[N] gives statistics for allocations of size S where
	// BySize[N-1].Size < S ≤ BySize[N].Size.
	//
	// This does not report allocations larger than BySize[60].Size.
	//
	// 按照大小进行的内存分配的统计,具体可以看Go内存分配的文章介绍.
	BySize [61]struct {
		// Size is the maximum byte size of an object in this
		// size class.
		Size uint32

		// Mallocs is the cumulative count of heap objects
		// allocated in this size class. The cumulative bytes
		// of allocation is Size*Mallocs. The number of live
		// objects in this size class is Mallocs - Frees.
		Mallocs uint64

		// Frees is the cumulative count of heap objects freed
		// in this size class.
		Frees uint64
	}
}

func init() {
	if offset := unsafe.Offsetof(memstats.heapStats); offset%8 != 0 {
		println(offset)
		throw("memstats.heapStats not aligned to 8 bytes")
	}
	// Ensure the size of heapStatsDelta causes adjacent fields/slots (e.g.
	// [3]heapStatsDelta) to be 8-byte aligned.
	if size := unsafe.Sizeof(heapStatsDelta{}); size%8 != 0 {
		println(size)
		throw("heapStatsDelta not a multiple of 8 bytes in size")
	}
}

// ReadMemStats populates m with memory allocator statistics.
//
// The returned memory allocator statistics are up to date as of the
// call to ReadMemStats. This is in contrast with a heap profile,
// which is a snapshot as of the most recently completed garbage
// collection cycle.
func ReadMemStats(m *MemStats) {
	stopTheWorld("read mem stats")

	systemstack(func() {
		readmemstats_m(m)
	})

	startTheWorld()
}

// readmemstats_m populates stats for internal runtime values.
//
// The world must be stopped.
func readmemstats_m(stats *MemStats) {
	assertWorldStopped()

	// Flush mcaches to mcentral before doing anything else.
	//
	// Flushing to the mcentral may in general cause stats to
	// change as mcentral data structures are manipulated.
	systemstack(flushallmcaches)

	// Calculate memory allocator stats.
	// During program execution we only count number of frees and amount of freed memory.
	// Current number of alive objects in the heap and amount of alive heap memory
	// are calculated by scanning all spans.
	// Total number of mallocs is calculated as number of frees plus number of alive objects.
	// Similarly, total amount of allocated memory is calculated as amount of freed memory
	// plus amount of alive heap memory.

	// Collect consistent stats, which are the source-of-truth in some cases.
	var consStats heapStatsDelta
	memstats.heapStats.unsafeRead(&consStats)

	// Collect large allocation stats.
	totalAlloc := consStats.largeAlloc
	nMalloc := consStats.largeAllocCount
	totalFree := consStats.largeFree
	nFree := consStats.largeFreeCount

	// Collect per-sizeclass stats.
	var bySize [_NumSizeClasses]struct {
		Size    uint32
		Mallocs uint64
		Frees   uint64
	}
	for i := range bySize {
		bySize[i].Size = uint32(class_to_size[i])

		// Malloc stats.
		a := consStats.smallAllocCount[i]
		totalAlloc += a * uint64(class_to_size[i])
		nMalloc += a
		bySize[i].Mallocs = a

		// Free stats.
		f := consStats.smallFreeCount[i]
		totalFree += f * uint64(class_to_size[i])
		nFree += f
		bySize[i].Frees = f
	}

	// Account for tiny allocations.
	// For historical reasons, MemStats includes tiny allocations
	// in both the total free and total alloc count. This double-counts
	// memory in some sense because their tiny allocation block is also
	// counted. Tracking the lifetime of individual tiny allocations is
	// currently not done because it would be too expensive.
	nFree += consStats.tinyAllocCount
	nMalloc += consStats.tinyAllocCount

	// Calculate derived stats.

	stackInUse := uint64(consStats.inStacks)
	gcWorkBufInUse := uint64(consStats.inWorkBufs)
	gcProgPtrScalarBitsInUse := uint64(consStats.inPtrScalarBits)

	totalMapped := gcController.heapInUse.load() + gcController.heapFree.load() + gcController.heapReleased.load() +
		memstats.stacks_sys.load() + memstats.mspan_sys.load() + memstats.mcache_sys.load() +
		memstats.buckhash_sys.load() + memstats.gcMiscSys.load() + memstats.other_sys.load() +
		stackInUse + gcWorkBufInUse + gcProgPtrScalarBitsInUse

	heapGoal := gcController.heapGoal()

	// The world is stopped, so the consistent stats (after aggregation)
	// should be identical to some combination of memstats. In particular:
	//
	// * memstats.heapInUse == inHeap
	// * memstats.heapReleased == released
	// * memstats.heapInUse + memstats.heapFree == committed - inStacks - inWorkBufs - inPtrScalarBits
	// * memstats.totalAlloc == totalAlloc
	// * memstats.totalFree == totalFree
	//
	// Check if that's actually true.
	//
	// TODO(mknyszek): Maybe don't throw here. It would be bad if a
	// bug in otherwise benign accounting caused the whole application
	// to crash.
	if gcController.heapInUse.load() != uint64(consStats.inHeap) {
		print("runtime: heapInUse=", gcController.heapInUse.load(), "\n")
		print("runtime: consistent value=", consStats.inHeap, "\n")
		throw("heapInUse and consistent stats are not equal")
	}
	if gcController.heapReleased.load() != uint64(consStats.released) {
		print("runtime: heapReleased=", gcController.heapReleased.load(), "\n")
		print("runtime: consistent value=", consStats.released, "\n")
		throw("heapReleased and consistent stats are not equal")
	}
	heapRetained := gcController.heapInUse.load() + gcController.heapFree.load()
	consRetained := uint64(consStats.committed - consStats.inStacks - consStats.inWorkBufs - consStats.inPtrScalarBits)
	if heapRetained != consRetained {
		print("runtime: global value=", heapRetained, "\n")
		print("runtime: consistent value=", consRetained, "\n")
		throw("measures of the retained heap are not equal")
	}
	if gcController.totalAlloc.Load() != totalAlloc {
		print("runtime: totalAlloc=", gcController.totalAlloc.Load(), "\n")
		print("runtime: consistent value=", totalAlloc, "\n")
		throw("totalAlloc and consistent stats are not equal")
	}
	if gcController.totalFree.Load() != totalFree {
		print("runtime: totalFree=", gcController.totalFree.Load(), "\n")
		print("runtime: consistent value=", totalFree, "\n")
		throw("totalFree and consistent stats are not equal")
	}
	// Also check that mappedReady lines up with totalMapped - released.
	// This isn't really the same type of "make sure consistent stats line up" situation,
	// but this is an opportune time to check.
	if gcController.mappedReady.Load() != totalMapped-uint64(consStats.released) {
		print("runtime: mappedReady=", gcController.mappedReady.Load(), "\n")
		print("runtime: totalMapped=", totalMapped, "\n")
		print("runtime: released=", uint64(consStats.released), "\n")
		print("runtime: totalMapped-released=", totalMapped-uint64(consStats.released), "\n")
		throw("mappedReady and other memstats are not equal")
	}

	// We've calculated all the values we need. Now, populate stats.

	stats.Alloc = totalAlloc - totalFree
	stats.TotalAlloc = totalAlloc
	stats.Sys = totalMapped
	stats.Mallocs = nMalloc
	stats.Frees = nFree
	stats.HeapAlloc = totalAlloc - totalFree
	stats.HeapSys = gcController.heapInUse.load() + gcController.heapFree.load() + gcController.heapReleased.load()
	// By definition, HeapIdle is memory that was mapped
	// for the heap but is not currently used to hold heap
	// objects. It also specifically is memory that can be
	// used for other purposes, like stacks, but this memory
	// is subtracted out of HeapSys before it makes that
	// transition. Put another way:
	//
	// HeapSys = bytes allocated from the OS for the heap - bytes ultimately used for non-heap purposes
	// HeapIdle = bytes allocated from the OS for the heap - bytes ultimately used for any purpose
	//
	// or
	//
	// HeapSys = sys - stacks_inuse - gcWorkBufInUse - gcProgPtrScalarBitsInUse
	// HeapIdle = sys - stacks_inuse - gcWorkBufInUse - gcProgPtrScalarBitsInUse - heapInUse
	//
	// => HeapIdle = HeapSys - heapInUse = heapFree + heapReleased
	stats.HeapIdle = gcController.heapFree.load() + gcController.heapReleased.load()
	stats.HeapInuse = gcController.heapInUse.load()
	stats.HeapReleased = gcController.heapReleased.load()
	stats.HeapObjects = nMalloc - nFree
	stats.StackInuse = stackInUse
	// memstats.stacks_sys is only memory mapped directly for OS stacks.
	// Add in heap-allocated stack memory for user consumption.
	stats.StackSys = stackInUse + memstats.stacks_sys.load()
	stats.MSpanInuse = uint64(mheap_.spanalloc.inuse)
	stats.MSpanSys = memstats.mspan_sys.load()
	stats.MCacheInuse = uint64(mheap_.cachealloc.inuse)
	stats.MCacheSys = memstats.mcache_sys.load()
	stats.BuckHashSys = memstats.buckhash_sys.load()
	// MemStats defines GCSys as an aggregate of all memory related
	// to the memory management system, but we track this memory
	// at a more granular level in the runtime.
	stats.GCSys = memstats.gcMiscSys.load() + gcWorkBufInUse + gcProgPtrScalarBitsInUse
	stats.OtherSys = memstats.other_sys.load()
	stats.NextGC = heapGoal
	stats.LastGC = memstats.last_gc_unix
	stats.PauseTotalNs = memstats.pause_total_ns
	stats.PauseNs = memstats.pause_ns
	stats.PauseEnd = memstats.pause_end
	stats.NumGC = memstats.numgc
	stats.NumForcedGC = memstats.numforcedgc
	stats.GCCPUFraction = memstats.gc_cpu_fraction
	stats.EnableGC = true

	// stats.BySize and bySize might not match in length.
	// That's OK, stats.BySize cannot change due to backwards
	// compatibility issues. copy will copy the minimum amount
	// of values between the two of them.
	copy(stats.BySize[:], bySize[:])
}

//go:linkname readGCStats runtime/debug.readGCStats
func readGCStats(pauses *[]uint64) {
	systemstack(func() {
		readGCStats_m(pauses)
	})
}

// readGCStats_m must be called on the system stack because it acquires the heap
// lock. See mheap for details.
//
//go:systemstack
func readGCStats_m(pauses *[]uint64) {
	p := *pauses
	// Calling code in runtime/debug should make the slice large enough.
	if cap(p) < len(memstats.pause_ns)+3 {
		throw("short slice passed to readGCStats")
	}

	// Pass back: pauses, pause ends, last gc (absolute time), number of gc, total pause ns.
	lock(&mheap_.lock)

	n := memstats.numgc
	if n > uint32(len(memstats.pause_ns)) {
		n = uint32(len(memstats.pause_ns))
	}

	// The pause buffer is circular. The most recent pause is at
	// pause_ns[(numgc-1)%len(pause_ns)], and then backward
	// from there to go back farther in time. We deliver the times
	// most recent first (in p[0]).
	p = p[:cap(p)]
	for i := uint32(0); i < n; i++ {
		j := (memstats.numgc - 1 - i) % uint32(len(memstats.pause_ns))
		p[i] = memstats.pause_ns[j]
		p[n+i] = memstats.pause_end[j]
	}

	p[n+n] = memstats.last_gc_unix
	p[n+n+1] = uint64(memstats.numgc)
	p[n+n+2] = memstats.pause_total_ns
	unlock(&mheap_.lock)
	*pauses = p[:n+n+3]
}

// flushmcache flushes the mcache of allp[i].
//
// The world must be stopped.
//
//go:nowritebarrier
func flushmcache(i int) {
	assertWorldStopped()

	p := allp[i]
	c := p.mcache
	if c == nil {
		return
	}
	c.releaseAll()
	stackcache_clear(c)
}

// flushallmcaches flushes the mcaches of all Ps.
//
// The world must be stopped.
//
//go:nowritebarrier
func flushallmcaches() {
	assertWorldStopped()

	for i := 0; i < int(gomaxprocs); i++ {
		flushmcache(i)
	}
}

// sysMemStat represents a global system statistic that is managed atomically.
//
// This type must structurally be a uint64 so that mstats aligns with MemStats.
type sysMemStat uint64

// load atomically reads the value of the stat.
//
// Must be nosplit as it is called in runtime initialization, e.g. newosproc0.
//
//go:nosplit
func (s *sysMemStat) load() uint64 {
	return atomic.Load64((*uint64)(s))
}

// add atomically adds the sysMemStat by n.
//
// Must be nosplit as it is called in runtime initialization, e.g. newosproc0.
//
//go:nosplit
func (s *sysMemStat) add(n int64) {
	val := atomic.Xadd64((*uint64)(s), n)
	if (n > 0 && int64(val) < n) || (n < 0 && int64(val)+n < n) {
		print("runtime: val=", val, " n=", n, "\n")
		throw("sysMemStat overflow")
	}
}

// heapStatsDelta contains deltas of various runtime memory statistics
// that need to be updated together in order for them to be kept
// consistent with one another.
type heapStatsDelta struct {
	// Memory stats.
	committed       int64 // byte delta of memory committed
	released        int64 // byte delta of released memory generated
	inHeap          int64 // byte delta of memory placed in the heap
	inStacks        int64 // byte delta of memory reserved for stacks
	inWorkBufs      int64 // byte delta of memory reserved for work bufs
	inPtrScalarBits int64 // byte delta of memory reserved for unrolled GC prog bits

	// Allocator stats.
	//
	// These are all uint64 because they're cumulative, and could quickly wrap
	// around otherwise.
	tinyAllocCount  uint64                  // number of tiny allocations
	largeAlloc      uint64                  // bytes allocated for large objects
	largeAllocCount uint64                  // number of large object allocations
	smallAllocCount [_NumSizeClasses]uint64 // number of allocs for small objects
	largeFree       uint64                  // bytes freed for large objects (>maxSmallSize)
	largeFreeCount  uint64                  // number of frees for large objects (>maxSmallSize)
	smallFreeCount  [_NumSizeClasses]uint64 // number of frees for small objects (<=maxSmallSize)

	// NOTE: This struct must be a multiple of 8 bytes in size because it
	// is stored in an array. If it's not, atomic accesses to the above
	// fields may be unaligned and fail on 32-bit platforms.
}

// merge adds in the deltas from b into a.
func (a *heapStatsDelta) merge(b *heapStatsDelta) {
	a.committed += b.committed
	a.released += b.released
	a.inHeap += b.inHeap
	a.inStacks += b.inStacks
	a.inWorkBufs += b.inWorkBufs
	a.inPtrScalarBits += b.inPtrScalarBits

	a.tinyAllocCount += b.tinyAllocCount
	a.largeAlloc += b.largeAlloc
	a.largeAllocCount += b.largeAllocCount
	for i := range b.smallAllocCount {
		a.smallAllocCount[i] += b.smallAllocCount[i]
	}
	a.largeFree += b.largeFree
	a.largeFreeCount += b.largeFreeCount
	for i := range b.smallFreeCount {
		a.smallFreeCount[i] += b.smallFreeCount[i]
	}
}

// consistentHeapStats represents a set of various memory statistics
// whose updates must be viewed completely to get a consistent
// state of the world.
//
// To write updates to memory stats use the acquire and release
// methods. To obtain a consistent global snapshot of these statistics,
// use read.
type consistentHeapStats struct {
	// stats is a ring buffer of heapStatsDelta values.
	// Writers always atomically update the delta at index gen.
	//
	// Readers operate by rotating gen (0 -> 1 -> 2 -> 0 -> ...)
	// and synchronizing with writers by observing each P's
	// statsSeq field. If the reader observes a P not writing,
	// it can be sure that it will pick up the new gen value the
	// next time it writes.
	//
	// The reader then takes responsibility by clearing space
	// in the ring buffer for the next reader to rotate gen to
	// that space (i.e. it merges in values from index (gen-2) mod 3
	// to index (gen-1) mod 3, then clears the former).
	//
	// Note that this means only one reader can be reading at a time.
	// There is no way for readers to synchronize.
	//
	// This process is why we need a ring buffer of size 3 instead
	// of 2: one is for the writers, one contains the most recent
	// data, and the last one is clear so writers can begin writing
	// to it the moment gen is updated.
	stats [3]heapStatsDelta

	// gen represents the current index into which writers
	// are writing, and can take on the value of 0, 1, or 2.
	gen atomic.Uint32

	// noPLock is intended to provide mutual exclusion for updating
	// stats when no P is available. It does not block other writers
	// with a P, only other writers without a P and the reader. Because
	// stats are usually updated when a P is available, contention on
	// this lock should be minimal.
	noPLock mutex
}

// acquire returns a heapStatsDelta to be updated. In effect,
// it acquires the shard for writing. release must be called
// as soon as the relevant deltas are updated.
//
// The returned heapStatsDelta must be updated atomically.
//
// The caller's P must not change between acquire and
// release. This also means that the caller should not
// acquire a P or release its P in between. A P also must
// not acquire a given consistentHeapStats if it hasn't
// yet released it.
//
// nosplit because a stack growth in this function could
// lead to a stack allocation that could reenter the
// function.
//
//go:nosplit
func (m *consistentHeapStats) acquire() *heapStatsDelta {
	if pp := getg().m.p.ptr(); pp != nil {
		seq := pp.statsSeq.Add(1)
		if seq%2 == 0 {
			// Should have been incremented to odd.
			print("runtime: seq=", seq, "\n")
			throw("bad sequence number")
		}
	} else {
		lock(&m.noPLock)
	}
	gen := m.gen.Load() % 3
	return &m.stats[gen]
}

// release indicates that the writer is done modifying
// the delta. The value returned by the corresponding
// acquire must no longer be accessed or modified after
// release is called.
//
// The caller's P must not change between acquire and
// release. This also means that the caller should not
// acquire a P or release its P in between.
//
// nosplit because a stack growth in this function could
// lead to a stack allocation that causes another acquire
// before this operation has completed.
//
//go:nosplit
func (m *consistentHeapStats) release() {
	if pp := getg().m.p.ptr(); pp != nil {
		seq := pp.statsSeq.Add(1)
		if seq%2 != 0 {
			// Should have been incremented to even.
			print("runtime: seq=", seq, "\n")
			throw("bad sequence number")
		}
	} else {
		unlock(&m.noPLock)
	}
}

// unsafeRead aggregates the delta for this shard into out.
//
// Unsafe because it does so without any synchronization. The
// world must be stopped.
func (m *consistentHeapStats) unsafeRead(out *heapStatsDelta) {
	assertWorldStopped()

	for i := range m.stats {
		out.merge(&m.stats[i])
	}
}

// unsafeClear clears the shard.
//
// Unsafe because the world must be stopped and values should
// be donated elsewhere before clearing.
func (m *consistentHeapStats) unsafeClear() {
	assertWorldStopped()

	for i := range m.stats {
		m.stats[i] = heapStatsDelta{}
	}
}

// read takes a globally consistent snapshot of m
// and puts the aggregated value in out. Even though out is a
// heapStatsDelta, the resulting values should be complete and
// valid statistic values.
//
// Not safe to call concurrently. The world must be stopped
// or metricsSema must be held.
func (m *consistentHeapStats) read(out *heapStatsDelta) {
	// Getting preempted after this point is not safe because
	// we read allp. We need to make sure a STW can't happen
	// so it doesn't change out from under us.
	mp := acquirem()

	// Get the current generation. We can be confident that this
	// will not change since read is serialized and is the only
	// one that modifies currGen.
	currGen := m.gen.Load()
	prevGen := currGen - 1
	if currGen == 0 {
		prevGen = 2
	}

	// Prevent writers without a P from writing while we update gen.
	lock(&m.noPLock)

	// Rotate gen, effectively taking a snapshot of the state of
	// these statistics at the point of the exchange by moving
	// writers to the next set of deltas.
	//
	// This exchange is safe to do because we won't race
	// with anyone else trying to update this value.
	m.gen.Swap((currGen + 1) % 3)

	// Allow P-less writers to continue. They'll be writing to the
	// next generation now.
	unlock(&m.noPLock)

	for _, p := range allp {
		// Spin until there are no more writers.
		for p.statsSeq.Load()%2 != 0 {
		}
	}

	// At this point we've observed that each sequence
	// number is even, so any future writers will observe
	// the new gen value. That means it's safe to read from
	// the other deltas in the stats buffer.

	// Perform our responsibilities and free up
	// stats[prevGen] for the next time we want to take
	// a snapshot.
	m.stats[currGen].merge(&m.stats[prevGen])
	m.stats[prevGen] = heapStatsDelta{}

	// Finally, copy out the complete delta.
	*out = m.stats[currGen]

	releasem(mp)
}

type cpuStats struct {
	// All fields are CPU time in nanoseconds computed by comparing
	// calls of nanotime. This means they're all overestimates, because
	// they don't accurately compute on-CPU time (so some of the time
	// could be spent scheduled away by the OS).

	gcAssistTime    int64 // GC assists
	gcDedicatedTime int64 // GC dedicated mark workers + pauses
	gcIdleTime      int64 // GC idle mark workers
	gcPauseTime     int64 // GC pauses (all GOMAXPROCS, even if just 1 is running)
	gcTotalTime     int64

	scavengeAssistTime int64 // background scavenger
	scavengeBgTime     int64 // scavenge assists
	scavengeTotalTime  int64

	idleTime int64 // Time Ps spent in _Pidle.
	userTime int64 // Time Ps spent in _Prunning or _Psyscall that's not any of the above.

	totalTime int64 // GOMAXPROCS * (monotonic wall clock time elapsed)
}
