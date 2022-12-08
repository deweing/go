// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// Provided by runtime via linkname.
func throw(string)
func fatal(string)

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
//
// In the terminology of the Go memory model,
// the n'th call to Unlock “synchronizes before” the m'th call to Lock
// for any n < m.
// A successful call to TryLock is equivalent to a call to Lock.
// A failed call to TryLock does not establish any “synchronizes before”
// relation at all.
type Mutex struct {
	state int32
	sema  uint32
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	// 持有锁标记
	mutexLocked = 1 << iota // mutex is locked
	// 唤醒标记
	mutexWoken
	// 饥饿标记
	mutexStarving
	// 通过右移3为的位运算，可计算waiter个数
	mutexWaiterShift = iota

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	//
	// 1ms，进入饥饿状态的等待时间
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// Fast path: grab unlocked mutex.
	//
	// 快速检测： 一下就获取到锁（幸运儿）
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	// 漫漫长路：尝试自旋竞争或饥饿状态下饥饿goroutine竞争
	m.lockSlow()
}

// TryLock tries to lock m and reports whether it succeeded.
//
// Note that while correct uses of TryLock do exist, they are rare,
// and use of TryLock is often a sign of a deeper problem
// in a particular use of mutexes.
func (m *Mutex) TryLock() bool {
	old := m.state
	if old&(mutexLocked|mutexStarving) != 0 {
		return false
	}

	// There may be a goroutine waiting for the mutex, but we are
	// running now and can try to grab the mutex before that
	// goroutine wakes up.
	if !atomic.CompareAndSwapInt32(&m.state, old, old|mutexLocked) {
		return false
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
	return true
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false //此goroutine的饥饿标记
	awoke := false    //唤醒标记
	iter := 0         //自旋次数
	old := m.state    //当前锁的状态
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		//
		// 锁在非饥饿状态，还没有释放，尝试自旋
		//
		// runtime_canSpin(i): see runtime/proc.go sync_runtime_canSpin()
		// - 1.自旋次数 < 4
		// - 2.必须是多核并且GOMAXPROCS > 1
		// - 3.至少有一个其他的正在运行的P 并且本地运行队列为空
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			// 判断当前goroutine是不是在唤醒状态
			// 尝试将当前锁的Woken状态设置为1，表示已被唤醒
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 自旋
			runtime_doSpin()
			iter++
			old = m.state //再次获取锁的状态，之后会检查是否锁被释放了
			continue
		}
		// old是锁当前的状态，new是期望的状态，已期于在后面CAS操作中更改锁的状态
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		// 如果当前锁不是饥饿模式，则将new的locked位设置为1，表示加锁
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// 如果当前锁已经被加锁或者处于饥饿模式，则将waiter数加1，表示申请上锁的goroutine直接去排队
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		//
		// 当前goroutine已饥饿，且Mutex是锁定状态，则标记为饥饿状态
		if starving && old&mutexLocked != 0 {
			new |= mutexStarving
		}
		// 当awoke=ture，修改锁的状态位为1
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				throw("sync: inconsistent mutex state")
			}
			new &^= mutexWoken //新状态清除唤醒标记
		}
		//成功设置新状态
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 原来锁的状态已释放，并且不是饥饿状态
			// 正常请求到了锁，返回
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 这里通过判断 waitStartTime!=0 就证明当前的goroutine之前就已经在等待队列里面了
			// 则需要将其加入到等待队列头
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}
			// 加入到等待队列头
			// 阻塞等待
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)
			// 唤醒之后，检查锁释放应该处于饥饿状态
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			old = m.state
			// 如果锁已经处于饥饿状态，直接抢到锁，返回
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.
				// 检查锁的状态
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				//加锁并且将waiter数减1
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// 最后一个waiter或者已经不饥饿了，清除饥饿标记
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			// 重置唤醒状态和自旋次数
			awoke = true
			iter = 0
		} else {
			// 如果CAS未成功，更新锁的状态，重新循环
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}

	// Fast path: drop lock bit.
	// 快速检查：解锁，判断如果没有等待的goroutine，直接返回
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {
		fatal("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 { //非饥饿模式
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			//
			// 如果锁没有waiter，或者锁有其他以下情况之一，则直接返回
			// 1.锁处于锁定状态，表示锁已经被其他goroutine获取了
			// 2.锁处于被唤醒状态，表示有等待goroutine被唤醒，不要尝试唤醒其他goroutine
			// 3.锁处于饥饿模式，那么锁之后会被直接交给等待队列队首的goroutine
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// Grab the right to wake someone.
			// 到这里，说明当前锁是空闲状态，等待队列中有waiter，且没有goroutine被唤醒
			// 所以，这里我们需要把锁的状态设置为唤醒，等待队列waiter数-1
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else { //饥饿模式
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// 直接唤醒等待队列队首的goroutine
		runtime_Semrelease(&m.sema, true, 1)
	}
}
