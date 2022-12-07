// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// A WaitGroup waits for a collection of goroutines to finish.
// The main goroutine calls Add to set the number of
// goroutines to wait for. Then each of the goroutines
// runs and calls Done when finished. At the same time,
// Wait can be used to block until all goroutines have finished.
//
// A WaitGroup must not be copied after first use.
//
// In the terminology of the Go memory model, a call to Done
// “synchronizes before” the return of any Wait call that it unblocks.
type WaitGroup struct {
	//避免复制，可以告诉vet工具违返了复制使用的规则
	noCopy noCopy

	// 高32位为任务计数值Counter
	// 低32位为等待次数（Waiter）
	state atomic.Uint64 // high 32 bits are counter, low 32 bits are waiter count.
	sema  uint32
}

// Add adds delta, which may be negative, to the WaitGroup counter.
// If the counter becomes zero, all goroutines blocked on Wait are released.
// If the counter goes negative, Add panics.
//
// Note that calls with a positive delta that occur when the counter is zero
// must happen before a Wait. Calls with a negative delta, or calls with a
// positive delta that start when the counter is greater than zero, may happen
// at any time.
// Typically this means the calls to Add should execute before the statement
// creating the goroutine or other event to be waited for.
// If a WaitGroup is reused to wait for several independent sets of events,
// new Add calls must happen after all previous Wait calls have returned.
// See the WaitGroup example.
func (wg *WaitGroup) Add(delta int) {
	if race.Enabled {
		if delta < 0 {
			// Synchronize decrements with Wait.
			race.ReleaseMerge(unsafe.Pointer(wg))
		}
		race.Disable()
		defer race.Enable()
	}
	// 计算新的state值，delta加在高32位
	state := wg.state.Add(uint64(delta) << 32)
	// 计数器数量
	v := int32(state >> 32)
	// 等待次数
	w := uint32(state)
	if race.Enabled && delta > 0 && v == int32(delta) {
		// The first increment must be synchronized with Wait.
		// Need to model this as a read, because there can be
		// several concurrent wg.counter transitions from 0.
		race.Read(unsafe.Pointer(&wg.sema))
	}
	// 如果等待任务总数小于0，直接panic
	if v < 0 {
		panic("sync: negative WaitGroup counter")
	}

	// 如果wait次数不等于0
	// 新加的任务数和任务总数又一样，直接panic
	// 错误的使用waitgroup, wait和add同时调用？
	if w != 0 && delta > 0 && v == int32(delta) {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// 任务是>0 或者 等待次数=0，添加成功
	// 返回
	if v > 0 || w == 0 {
		return
	}
	// This goroutine has set counter to 0 when waiters > 0.
	// Now there can't be concurrent mutations of state:
	// - Adds must not happen concurrently with Wait,
	// - Wait does not increment waiters if it sees counter == 0.
	// Still do a cheap sanity check to detect WaitGroup misuse.
	//
	// 当waiter>0时，当前goroutine将计数器设置为0
	// 现在不能再有并发的状态发生了
	// - Adds(delta) 和 Wait 不能同时发生
	// - 如果 Wait 发现 counter==0，它就不会增加等待次数
	// 所以对WaitGroup的误用做一个判断
	if wg.state.Load() != state {
		panic("sync: WaitGroup misuse: Add called concurrently with Wait")
	}
	// Reset waiters count to 0.
	// 重置Waiter次数为0（Counter==0，上面已经判断过了）
	wg.state.Store(0)
	// 计数器为0，等待数不为0，释放信号量，唤醒wait
	for ; w != 0; w-- {
		runtime_Semrelease(&wg.sema, false, 0)
	}
}

// Done decrements the WaitGroup counter by one.
func (wg *WaitGroup) Done() {
	wg.Add(-1)
}

// Wait blocks until the WaitGroup counter is zero.
func (wg *WaitGroup) Wait() {
	if race.Enabled {
		race.Disable()
	}
	for {
		state := wg.state.Load()
		v := int32(state >> 32)
		w := uint32(state)
		// 如果计数器等于0
		// 返回
		if v == 0 {
			// Counter is 0, no need to wait.
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
		// Increment waiters count.
		// 更新等待次数
		if wg.state.CompareAndSwap(state, state+1) {
			if race.Enabled && w == 0 {
				// Wait must be synchronized with the first Add.
				// Need to model this is as a write to race with the read in Add.
				// As a consequence, can do the write only for the first waiter,
				// otherwise concurrent Waits will race with each other.
				race.Write(unsafe.Pointer(&wg.sema))
			}
			//阻塞休眠等待
			//获取到信号，直接退出
			runtime_Semacquire(&wg.sema)
			if wg.state.Load() != 0 {
				panic("sync: WaitGroup is reused before previous Wait has returned")
			}
			if race.Enabled {
				race.Enable()
				race.Acquire(unsafe.Pointer(wg))
			}
			return
		}
	}
}
