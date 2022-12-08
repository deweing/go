// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"runtime"
	"sync/atomic"
	"unsafe"
)

// A Pool is a set of temporary objects that may be individually saved and
// retrieved.
//
// Any item stored in the Pool may be removed automatically at any time without
// notification. If the Pool holds the only reference when this happens, the
// item might be deallocated.
//
// A Pool is safe for use by multiple goroutines simultaneously.
//
// Pool's purpose is to cache allocated but unused items for later reuse,
// relieving pressure on the garbage collector. That is, it makes it easy to
// build efficient, thread-safe free lists. However, it is not suitable for all
// free lists.
//
// An appropriate use of a Pool is to manage a group of temporary items
// silently shared among and potentially reused by concurrent independent
// clients of a package. Pool provides a way to amortize allocation overhead
// across many clients.
//
// An example of good use of a Pool is in the fmt package, which maintains a
// dynamically-sized store of temporary output buffers. The store scales under
// load (when many goroutines are actively printing) and shrinks when
// quiescent.
//
// On the other hand, a free list maintained as part of a short-lived object is
// not a suitable use for a Pool, since the overhead does not amortize well in
// that scenario. It is more efficient to have such objects implement their own
// free list.
//
// A Pool must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Put(x) “synchronizes before”
// a call to Get returning that same value x.
// Similarly, a call to New returning x “synchronizes before”
// a call to Get returning that same value x.
//
// Pool使用总结
// 1.Pool本质是为了提高临时对象的复用率；
// 2.Pool使用两层回收策略（local+victim）避免性能波动；
// 3.Pool本质是一个杂货铺属性，啥都可以放。把什么东西放进去，预期从里面拿出什么类型的东西都需要业务方使用把控，Pool池本身不做限制；
// 4.Pool池里面cache对象也是分层的，一层层的cache，取用方式从最热的数据到最冷的数据投递；
// 5.Pool是并发安全的，但是内部是无锁结构，原理是对每个P都分配cache数组(poolLocalInternal数组)，这样cache结构就不会导致并发；
// 6.永远不要copy一个Pool，明确禁止，不然会导致内存泄露和程序并发逻辑错误；
// 7.代码编译之前用 go vet 做静态检查，能减少非常多的问题
// 8.每轮GC开始都会清理一把Pool里面cache的对象，注意流程是分两步，当前Pool池local数组里的元素交给victim数组句柄，victim里面cache的元素全部清理。换句话说，引入 victim 机制之后，对象的缓存时间变成两个GC周期；
// 9.不能对Pool.Get出来的对象做预判, 有可能是新的（新分配的），有可能是旧的（之前人用过，然后Put进去的）
// 10.当用完一个从pool取出来的实例后，一定要记得调用Put，否则pool无法复用这个实例，通常用defer完成；
// 11.不能对pool池里面的元素个数做假定，你不能够
type Pool struct {
	// 通过vet检查保证不能被复制
	noCopy noCopy

	// 每个p的本地队列，实际类型为[P]poolLocal
	local unsafe.Pointer // local fixed-size per-P pool, actual type is [P]poolLocal
	// [P]poolLocal的大小
	localSize uintptr // size of the local array

	// GC 到时，victim 和 victimSize 会分别接管 local 和 localSize；
	// victim 的目的是为了减少 GC 后冷启动导致的性能抖动，让分配对象更平滑
	victim     unsafe.Pointer // local from previous cycle
	victimSize uintptr        // size of victims array

	// New optionally specifies a function to generate
	// a value when Get would otherwise return nil.
	// It may not be changed concurrently with calls to Get.
	// 自定义的对象创建回调函数，当pool中无可用对象时会调用此函数
	New func() any
}

// Local per-P Pool appendix.
// 管理 cache 的内部结构，跟每个 P 对应，操作无需加锁
type poolLocalInternal struct {
	//P的私有缓存区，使用时不需要加锁
	private any // Can be used only by the respective P.
	// 公共缓存区，本地P可以 pushHead/popHead，其他P则只能 popTail
	// 双向链表
	shared poolChain // Local P can pushHead/popHead; any P can popTail.
}

type poolLocal struct {
	poolLocalInternal

	// Prevents false sharing on widespread platforms with
	// 128 mod (cache line size) = 0 .
	//
	// 把 poolLocal 填充至 128 字节对齐，避免 false sharing 引起的性能问题
	//
	// 在大多数平台上，128 mod (cache line size) = 0 可以防止伪共享
	// 伪共享，仅占位用，防止在cache line size 上分配多个 poolLocalInternal
	pad [128 - unsafe.Sizeof(poolLocalInternal{})%128]byte
}

// from runtime
func fastrandn(n uint32) uint32

var poolRaceHash [128]uint64

// poolRaceAddr returns an address to use as the synchronization point
// for race detector logic. We don't use the actual pointer stored in x
// directly, for fear of conflicting with other synchronization on that address.
// Instead, we hash the pointer to get an index into poolRaceHash.
// See discussion on golang.org/cl/31589.
func poolRaceAddr(x any) unsafe.Pointer {
	ptr := uintptr((*[2]unsafe.Pointer)(unsafe.Pointer(&x))[1])
	h := uint32((uint64(uint32(ptr)) * 0x85ebca6b) >> 16)
	return unsafe.Pointer(&poolRaceHash[h%uint32(len(poolRaceHash))])
}

// Put adds x to the pool.
// Put一个元素进池子
func (p *Pool) Put(x any) {
	if x == nil {
		return
	}
	if race.Enabled {
		if fastrandn(4) == 0 {
			// Randomly drop x on floor.
			return
		}
		race.ReleaseMerge(poolRaceAddr(x))
		race.Disable()
	}
	// G-M 锁定
	l, _ := p.pin()
	if l.private == nil {
		// 尝试放到最快的位置
		l.private = x
	} else {
		// 放到双向链表中
		l.shared.pushHead(x)
	}
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
	}
}

// Get selects an arbitrary item from the Pool, removes it from the
// Pool, and returns it to the caller.
// Get may choose to ignore the pool and treat it as empty.
// Callers should not assume any relation between values passed to Put and
// the values returned by Get.
//
// If Get would otherwise return nil and p.New is non-nil, Get returns
// the result of calling p.New.
//
// Get 从 Pool 池里取一个元素出来，元素是层层 cache 的，由最快到最慢一层层尝试
// 最快的是本 P 对应的列表里通过 private  字段直接取出，最慢的就是调用 New 函数现场构造。
//
// 尝试路径：
// 1.当前 P 对应的 local.private 字段
// 2.当前 P 对应的 local 的双向链表
// 3.其他 P 对应的 local 列表
// 4.victim cache 里的元素
// 5.New 现场构造；
func (p *Pool) Get() any {
	if race.Enabled {
		race.Disable()
	}

	// 把G锁住在当前M (声明当前 M 不能被抢占)，返回M绑定的P的ID
	// 在当前场景，也可以认为是G绑定到P，因为这种场景P不可能被抢占，只有系统调用的时候才有P被抢占的场景
	l, pid := p.pin()
	// 如果能从private取出缓存的元素，那么将是最快的路径
	x := l.private
	l.private = nil
	if x == nil {
		// Try to pop the head of the local shard. We prefer
		// the head over the tail for temporal locality of
		// reuse.
		// 从shared队列里获取
		x, _ = l.shared.popHead()
		if x == nil {
			//尝试从其他P的队列获取，或者尝试从 victim cache 里面获取
			x = p.getSlow(pid)
		}
	}
	// G-M锁定解除
	runtime_procUnpin()
	if race.Enabled {
		race.Enable()
		if x != nil {
			race.Acquire(poolRaceAddr(x))
		}
	}
	//最慢的路径：现场初始化，这种场景是 Pool 池里面一个对象都没有，只能初始化
	if x == nil && p.New != nil {
		x = p.New()
	}
	//返回对象
	return x
}

func (p *Pool) getSlow(pid int) any {
	// See the comment in pin regarding ordering of the loads.
	size := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	locals := p.local                            // load-consume
	// Try to steal one element from other procs.
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i+1)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Try the victim cache. We do this after attempting to steal
	// from all primary caches because we want objects in the
	// victim cache to age out if at all possible.
	size = atomic.LoadUintptr(&p.victimSize)
	if uintptr(pid) >= size {
		return nil
	}
	locals = p.victim
	l := indexLocal(locals, pid)
	if x := l.private; x != nil {
		l.private = nil
		return x
	}
	for i := 0; i < int(size); i++ {
		l := indexLocal(locals, (pid+i)%int(size))
		if x, _ := l.shared.popTail(); x != nil {
			return x
		}
	}

	// Mark the victim cache as empty for future gets don't bother
	// with it.
	atomic.StoreUintptr(&p.victimSize, 0)

	return nil
}

// pin pins the current goroutine to P, disables preemption and
// returns poolLocal pool for the P and the P's id.
// Caller must call runtime_procUnpin() when done with the pool.
//
// pin 作用就是将当前的goroutine和P绑定在一起，禁止抢占
// 并且返回对应的poolLocal已经P的id
// 调用方必须在完成取值后调用 runtime_procUnpin()方法来取消抢占
func (p *Pool) pin() (*poolLocal, int) {
	pid := runtime_procPin()
	// In pinSlow we store to local and then to localSize, here we load in opposite order.
	// Since we've disabled preemption, GC cannot happen in between.
	// Thus here we must observe local at least as large localSize.
	// We can observe a newer/larger local, it is fine (we must observe its zero-initialized-ness).
	s := runtime_LoadAcquintptr(&p.localSize) // load-acquire
	l := p.local                              // load-consume
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	return p.pinSlow()
}

// 1.首次 Pool 需要把自己注册进 allPools 数组
// 2.Pool.local 数组按照 runtime.GOMAXPROCS(0) 的大小进行分配，如果是默认的，那么这个就是 P 的个数，也就是 CPU 的个数
func (p *Pool) pinSlow() (*poolLocal, int) {
	// Retry under the mutex.
	// Can not lock the mutex while pinned.
	// G-M 先解锁
	runtime_procUnpin()
	// 以下逻辑在全局锁 allPoolsMu 内
	allPoolsMu.Lock()
	defer allPoolsMu.Unlock()
	// 获取当前 G-M-P ，P 的 id
	pid := runtime_procPin()
	// poolCleanup won't be called while we are pinned.
	s := p.localSize
	l := p.local
	if uintptr(pid) < s {
		return indexLocal(l, pid), pid
	}
	if p.local == nil {
		// 首次，Pool 需要把自己注册进 allPools 数组
		allPools = append(allPools, p)
	}
	// If GOMAXPROCS changes between GCs, we re-allocate the array and lose the old one.
	// P 的个数
	size := runtime.GOMAXPROCS(0)
	// local 数组的大小就等于 runtime.GOMAXPROCS(0)
	local := make([]poolLocal, size)
	atomic.StorePointer(&p.local, unsafe.Pointer(&local[0])) // store-release
	runtime_StoreReluintptr(&p.localSize, uintptr(size))     // store-release
	return &local[pid], pid
}

func poolCleanup() {
	// This function is called with the world stopped, at the beginning of a garbage collection.
	// It must not allocate and probably should not call any runtime functions.

	// Because the world is stopped, no pool user can be in a
	// pinned section (in effect, this has all Ps pinned).

	// Drop victim caches from all pools.
	// 清理oldPools上的victim的元素
	for _, p := range oldPools {
		p.victim = nil
		p.victimSize = 0
	}

	// Move primary cache to victim cache.
	// 把local cache 迁移到 victim 上
	// 这样就不致于让GC把所有的Pool都清空了，有victim再兜底下，这样可以防止抖动；
	for _, p := range allPools {
		p.victim = p.local
		p.victimSize = p.localSize
		p.local = nil
		p.localSize = 0
	}

	// The pools with non-empty primary caches now have non-empty
	// victim caches and no pools have primary caches.
	// 清理 allPools
	oldPools, allPools = allPools, nil
}

var (
	allPoolsMu Mutex

	// allPools is the set of pools that have non-empty primary
	// caches. Protected by either 1) allPoolsMu and pinning or 2)
	// STW.
	allPools []*Pool

	// oldPools is the set of pools that may have non-empty victim
	// caches. Protected by STW.
	oldPools []*Pool
)

func init() {
	runtime_registerPoolCleanup(poolCleanup)
}

func indexLocal(l unsafe.Pointer, i int) *poolLocal {
	lp := unsafe.Pointer(uintptr(l) + uintptr(i)*unsafe.Sizeof(poolLocal{}))
	return (*poolLocal)(lp)
}

// Implemented in runtime.
func runtime_registerPoolCleanup(cleanup func())
func runtime_procPin() int
func runtime_procUnpin()

// The below are implemented in runtime/internal/atomic and the
// compiler also knows to intrinsify the symbol we linkname into this
// package.

//go:linkname runtime_LoadAcquintptr runtime/internal/atomic.LoadAcquintptr
func runtime_LoadAcquintptr(ptr *uintptr) uintptr

//go:linkname runtime_StoreReluintptr runtime/internal/atomic.StoreReluintptr
func runtime_StoreReluintptr(ptr *uintptr, val uintptr) uintptr
