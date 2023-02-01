// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go channels.

// Invariants:
//  At least one of c.sendq and c.recvq is empty,
//  except for the case of an unbuffered channel with a single goroutine
//  blocked on it for both sending and receiving using a select statement,
//  in which case the length of c.sendq and c.recvq is limited only by the
//  size of the select statement.
//
// For buffered channels, also:
//  c.qcount > 0 implies that c.recvq is empty.
//  c.qcount < c.dataqsiz implies that c.sendq is empty.

import (
	"internal/abi"
	"runtime/internal/atomic"
	"runtime/internal/math"
	"unsafe"
)

const (
	maxAlign  = 8
	hchanSize = unsafe.Sizeof(hchan{}) + uintptr(-int(unsafe.Sizeof(hchan{}))&(maxAlign-1))
	debugChan = false
)

type hchan struct {
	// channel里面的元素数量
	qcount uint // total data in the queue
	// channel底层循环数组的长度
	dataqsiz uint // size of the circular queue
	// 指向底层循环数组的指针
	// 只针对有缓冲的channel
	buf unsafe.Pointer // points to an array of dataqsiz elements
	// channel中的元素大小，缓冲队列
	elemsize uint16
	// channel是否被关闭
	closed uint32
	// channel中的元素类型
	elemtype *_type // element type
	// 已发送元素在循环数组中的索引
	sendx uint // send index
	// 已结束元素在循环数组中的索引
	recvx uint // receive index
	// 等待接收的 groutine 队列（双向链表）
	recvq waitq // list of recv waiters
	// 等待发送的 groutine 队列（双向链表）
	sendq waitq // list of send waiters

	// lock protects all fields in hchan, as well as several
	// fields in sudogs blocked on this channel.
	//
	// Do not change another G's status while holding this lock
	// (in particular, do not ready a G), as this can deadlock
	// with stack shrinking.
	// 保护 hchan 中的所有字段
	lock mutex
}

// 双向链表
type waitq struct {
	first *sudog //sudog 对goroutine的一个封装
	last  *sudog
}

//go:linkname reflect_makechan reflect.makechan
func reflect_makechan(t *chantype, size int) *hchan {
	return makechan(t, size)
}

func makechan64(t *chantype, size int64) *hchan {
	if int64(int(size)) != size {
		panic(plainError("makechan: size out of range"))
	}

	return makechan(t, int(size))
}

func makechan(t *chantype, size int) *hchan {
	elem := t.elem

	// compiler checks this but be safe.
	if elem.size >= 1<<16 {
		throw("makechan: invalid channel element type")
	}
	if hchanSize%maxAlign != 0 || elem.align > maxAlign {
		throw("makechan: bad alignment")
	}

	//计算缓冲区需要的总大小(缓冲区大小*元素大小)，并判断是否超出最大的可分配范围
	mem, overflow := math.MulUintptr(elem.size, uintptr(size))
	if overflow || mem > maxAlloc-hchanSize || size < 0 {
		panic(plainError("makechan: size out of range"))
	}

	// Hchan does not contain pointers interesting for GC when elements stored in buf do not contain pointers.
	// buf points into the same allocation, elemtype is persistent.
	// SudoG's are referenced from their owning thread so they can't be collected.
	// TODO(dvyukov,rlh): Rethink when collector can move allocated objects.
	var c *hchan
	switch {
	case mem == 0:
		// Queue or element size is zero.
		// 缓冲区大小为0，或者channel中的元素大小为0(struct{}{})时，只需要分配channel必须的空间
		c = (*hchan)(mallocgc(hchanSize, nil, true))
		// Race detector uses this location for synchronization.
		c.buf = c.raceaddr()
	case elem.ptrdata == 0:
		// Elements do not contain pointers.
		// Allocate hchan and buf in one call.
		// channel中的元素类型不是指针，分配一片连续内存空间，所需空间等于缓存区数组空间+ hchan必须的空间
		c = (*hchan)(mallocgc(hchanSize+mem, nil, true))
		c.buf = add(unsafe.Pointer(c), hchanSize)
	default:
		// Elements contain pointers.
		// 元素中包含指针，为chan和缓冲区分配空间
		c = new(hchan)
		c.buf = mallocgc(mem, elem, true)
	}

	c.elemsize = uint16(elem.size)
	c.elemtype = elem
	// 底层循环数组长度
	c.dataqsiz = uint(size)
	lockInit(&c.lock, lockRankHchan)

	if debugChan {
		print("makechan: chan=", c, "; elemsize=", elem.size, "; dataqsiz=", size, "\n")
	}
	return c
}

// chanbuf(c, i) is pointer to the i'th slot in the buffer.
func chanbuf(c *hchan, i uint) unsafe.Pointer {
	return add(c.buf, uintptr(i)*uintptr(c.elemsize))
}

// full reports whether a send on c would block (that is, the channel is full).
// It uses a single word-sized read of mutable state, so although
// the answer is instantaneously true, the correct answer may have changed
// by the time the calling function receives the return value.
// 判断缓冲区是否已满
func full(c *hchan) bool {
	// c.dataqsiz is immutable (never written after the channel is created)
	// so it is safe to read at any time during channel operation.
	if c.dataqsiz == 0 {
		// Assumes that a pointer read is relaxed-atomic.
		// 没有缓冲区且没有等待接收的Groutine
		return c.recvq.first == nil
	}
	// Assumes that a uint read is relaxed-atomic.
	// 有缓冲区且缓冲区已满
	return c.qcount == c.dataqsiz
}

// entry point for c <- x from compiled code.
//
//go:nosplit
func chansend1(c *hchan, elem unsafe.Pointer) {
	chansend(c, elem, true, getcallerpc())
}

/*
 * generic single channel send/recv
 * If block is not nil,
 * then the protocol will not
 * sleep but return if it could
 * not complete.
 *
 * sleep can wake up with g.param == nil
 * when a channel involved in the sleep has
 * been closed.  it is easiest to loop and re-run
 * the operation; we'll see that it's now closed.
 */
func chansend(c *hchan, ep unsafe.Pointer, block bool, callerpc uintptr) bool {
	// 如果channel是nil
	if c == nil {
		// 非阻塞，直接返回false，表示发送未成功
		if !block {
			return false
		}
		// 否则，当前goroutine被阻塞挂起
		gopark(nil, nil, waitReasonChanSendNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	if debugChan {
		print("chansend: chan=", c, "\n")
	}

	if raceenabled {
		racereadpc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(chansend))
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	//
	// After observing that the channel is not closed, we observe that the channel is
	// not ready for sending. Each of these observations is a single word-sized read
	// (first c.closed and second full()).
	// Because a closed channel cannot transition from 'ready for sending' to
	// 'not ready for sending', even if the channel is closed between the two observations,
	// they imply a moment between the two when the channel was both not yet closed
	// and not ready for sending. We behave as if we observed the channel at that moment,
	// and report that the send cannot proceed.
	//
	// It is okay if the reads are reordered here: if we observe that the channel is not
	// ready for sending and then observe that it is not closed, that implies that the
	// channel wasn't closed during the first observation. However, nothing here
	// guarantees forward progress. We rely on the side effects of lock release in
	// chanrecv() and closechan() to update this thread's view of c.closed and full().
	//
	// 快速检查：对于非阻塞且channel未关闭，如果无缓冲区且没有等待接收的Goroutine,或者有缓冲区且缓冲区已满，
	// 那么直接返回发生不成功 （不能阻塞、并且队列已满）
	if !block && c.closed == 0 && full(c) {
		return false
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	// 锁住channel，并发安全
	lock(&c.lock)

	// 如果chanel已经关闭
	if c.closed != 0 {
		unlock(&c.lock) //解锁
		// 直接panic
		panic(plainError("send on closed channel"))
	}

	// 除了以上情况，当channel未关闭时，就有以下几种情况

	// 1.如果接收（等待）队列里面有 goroutine，直接将要发送的数据拷贝到接收的goroutine
	if sg := c.recvq.dequeue(); sg != nil {
		// Found a waiting receiver. We pass the value we want to send
		// directly to the receiver, bypassing the channel buffer (if any).
		send(c, sg, ep, func() { unlock(&c.lock) }, 3)
		return true
	}

	// 2. 对于缓冲队列，当缓冲区未满时
	if c.qcount < c.dataqsiz {
		// Space is available in the channel buffer. Enqueue the element to send.
		// 获取指向缓冲区数组（循环队列c.buf）中位于sendx位置的元素指针
		qp := chanbuf(c, c.sendx)
		if raceenabled {
			racenotify(c, c.sendx, nil)
		}
		// 将当前发送的数据拷贝到缓冲区
		typedmemmove(c.elemtype, qp, ep)
		// 发送游标（sendx）加一
		c.sendx++
		// 因为是循环队列，如果发生游标值等于容量，游标值归0
		if c.sendx == c.dataqsiz {
			c.sendx = 0
		}
		// 队列中元素总数加一，并解锁，返回成功
		c.qcount++
		unlock(&c.lock)
		return true
	}

	// 3.当既没有等待接收的Goroutine,缓冲区也没有剩余空间，如果是非阻塞的发送，那么直接解锁，返回发送失败
	if !block {
		unlock(&c.lock)
		return false
	}

	// Block on the channel. Some receiver will complete our operation for us.
	// 4.如果是阻塞发送，那么就将当前的Goroutine打包成一个sudog结构体，并加入到channel的发送队列sendq里

	// 获取当前的gorotiune指针
	gp := getg()
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	mysg.elem = ep
	mysg.waitlink = nil
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.waiting = mysg
	gp.param = nil
	//当前的 goroutine 加入发送等待队列
	c.sendq.enqueue(mysg)
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	//
	// 当前goroutine设置为等待状态并解锁，进入休眠等待被唤醒
	gp.parkingOnChan.Store(true)
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanSend, traceEvGoBlockSend, 2)
	// Ensure the value being sent is kept alive until the
	// receiver copies it out. The sudog has a pointer to the
	// stack object, but sudogs aren't considered as roots of the
	// stack tracer.
	KeepAlive(ep)

	// someone woke us up.
	// 从这里开始被唤醒了 （channel 有机会可以发送了）
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	// 被唤醒之后执行清理工作并释放sudog结构体
	gp.waiting = nil
	gp.activeStackChans = false
	closed := !mysg.success
	gp.param = nil
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	mysg.c = nil
	releaseSudog(mysg)
	if closed {
		if c.closed == 0 {
			throw("chansend: spurious wakeup")
		}
		// 被唤醒后，channel关闭了，直接panic
		panic(plainError("send on closed channel"))
	}
	return true
}

// send processes a send operation on an empty channel c.
// The value ep sent by the sender is copied to the receiver sg.
// The receiver is then woken up to go on its merry way.
// Channel c must be empty and locked.  send unlocks c with unlockf.
// sg must already be dequeued from c.
// ep must be non-nil and point to the heap or the caller's stack.
//
// send 函数处理向一个空的channel发送元素

// ep指向发送的元素，会被直接复制到接收的goroutine
// 之后，接收元素的goroutine会被唤醒
// c channel必须是空的（因为等待队列里面有goroutine，肯定是空的）
// c 必须被上锁，发送操作指令完成后，会使用unlockf函数解锁
// sg 必须已经从等待队列里面取出来
// ep 必须是非空值，并且指向堆或者调用者的栈
func send(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	//一些竞态检查
	if raceenabled {
		if c.dataqsiz == 0 {
			racesync(c, sg)
		} else {
			// Pretend we go through the buffer, even though
			// we copy directly. Note that we need to increment
			// the head/tail locations only when raceenabled.
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
			c.recvx++
			if c.recvx == c.dataqsiz {
				c.recvx = 0
			}
			c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
		}
	}
	// sg.elem 指向接收到的值存放的位置，如 val <-ch, 指的就是&val
	if sg.elem != nil {
		//直接复制内存（从发送者到接收者）
		sendDirect(c.elemtype, sg, ep)
		sg.elem = nil
	}
	gp := sg.g //sudog上绑定的goroutine
	unlockf()  //解锁
	gp.param = unsafe.Pointer(sg)
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	goready(gp, skip+1) //唤醒接收的 goroutine skip 和打印栈相关
}

// Sends and receives on unbuffered or empty-buffered channels are the
// only operations where one running goroutine writes to the stack of
// another running goroutine. The GC assumes that stack writes only
// happen when the goroutine is running and are only done by that
// goroutine. Using a write barrier is sufficient to make up for
// violating that assumption, but the write barrier has to work.
// typedmemmove will call bulkBarrierPreWrite, but the target bytes
// are not in the heap, so that will not help. We arrange to call
// memmove and typeBitsBulkBarrier instead.
//
// 向一个非缓冲型的channel发送数据，从一个无元素的（非缓冲型或缓存型但空）的channel
// 接收数据，都会导致一个goroutine直接操作另外一个goroutine的栈
// 由于GC假设对栈的写操作只能发生在goroutine正在运行并且由当前goroutine来写
// 所以这里实际违反了这个假设，可能会造成一些问题，所以需要用到写屏障来规避
func sendDirect(t *_type, sg *sudog, src unsafe.Pointer) {
	// src is on our stack, dst is a slot on another stack.
	// src在当前的 goroutine 栈上，dst是在另一个 goroutine 的栈

	// Once we read sg.elem out of sg, it will no longer
	// be updated if the destination's stack gets copied (shrunk).
	// So make sure that no preemption points can happen between read & use.
	// 直接进行内存“搬迁”
	// 如果目标地址的栈发生了栈搜索，当读出了 sg.elem 后
	// 就不能修改真正的dst位置了
	// 因此需要在读和写之前加上一个屏障
	dst := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	// No need for cgo write barrier checks because dst is always
	// Go memory.
	memmove(dst, src, t.size)
}

func recvDirect(t *_type, sg *sudog, dst unsafe.Pointer) {
	// dst is on our stack or the heap, src is on another stack.
	// The channel is locked, so src will not move during this
	// operation.
	src := sg.elem
	typeBitsBulkBarrier(t, uintptr(dst), uintptr(src), t.size)
	memmove(dst, src, t.size)
}

func closechan(c *hchan) {
	// nil检查，关闭nil的channel直接panic
	if c == nil {
		panic(plainError("close of nil channel"))
	}

	lock(&c.lock)
	//已经关闭的channel不能再次关闭，直接panic
	if c.closed != 0 {
		unlock(&c.lock)
		panic(plainError("close of closed channel"))
	}

	if raceenabled {
		callerpc := getcallerpc()
		racewritepc(c.raceaddr(), callerpc, abi.FuncPCABIInternal(closechan))
		racerelease(c.raceaddr())
	}

	// 设置关闭状态为1
	c.closed = 1

	var glist gList

	// release all readers
	// 遍历recvq，清除sudog的数据，取出其中处于_Gwaiting状态的Goroutine加入到glist中
	for {
		sg := c.recvq.dequeue()
		if sg == nil {
			break
		}
		if sg.elem != nil {
			typedmemclr(c.elemtype, sg.elem)
			sg.elem = nil
		}
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}

	// release all writers (they will panic)
	// 遍历sendq，清除sudog的数据，取出其中处于_Gwating状态的Goroutine加入到glist中
	for {
		sg := c.sendq.dequeue()
		if sg == nil {
			break
		}
		sg.elem = nil
		if sg.releasetime != 0 {
			sg.releasetime = cputicks()
		}
		gp := sg.g
		gp.param = unsafe.Pointer(sg)
		sg.success = false
		if raceenabled {
			raceacquireg(gp, c.raceaddr())
		}
		glist.push(gp)
	}
	unlock(&c.lock)

	// Ready all Gs now that we've dropped the channel lock.
	// 将glist中所有goroutine的状态置为_Grunnable中，等待调度器进行调度
	for !glist.empty() {
		gp := glist.pop()
		gp.schedlink = 0
		goready(gp, 3)
	}
}

// empty reports whether a read from c would block (that is, the channel is
// empty).  It uses a single atomic read of mutable state.
// 判断channel是否为空
func empty(c *hchan) bool {
	// c.dataqsiz is immutable.
	if c.dataqsiz == 0 {
		// 无缓冲区且等待发送队列也为空
		return atomic.Loadp(unsafe.Pointer(&c.sendq.first)) == nil
	}
	// 有缓冲区但缓冲区数组为空
	return atomic.Loaduint(&c.qcount) == 0
}

// entry points for <- c from compiled code.
//
//go:nosplit
func chanrecv1(c *hchan, elem unsafe.Pointer) {
	chanrecv(c, elem, true)
}

//go:nosplit
func chanrecv2(c *hchan, elem unsafe.Pointer) (received bool) {
	_, received = chanrecv(c, elem, true)
	return
}

// chanrecv receives on channel c and writes the received data to ep.
// ep may be nil, in which case received data is ignored.
// If block == false and no elements are available, returns (false, false).
// Otherwise, if c is closed, zeros *ep and returns (true, false).
// Otherwise, fills in *ep with an element and returns (true, true).
// A non-nil ep must point to the heap or the caller's stack.
//
// chanrecv 接收 channel c 的元素并且将其写入ep所指向的内存地址
// 如果ep是nil，说明调用者忽略了接收值
// 如果 block == false 即非阻塞型接收，在没有数据可以接受的情况下，返回(false, false)
// 否则，如果 channel已经关闭，将ep指向的地址清零，返回(true, false)
// 否则，用返回值填充ep指向的内存地址，返回（true, true）
// 如果 ep 非空，则应该指向堆或者函数调用者的栈
func chanrecv(c *hchan, ep unsafe.Pointer, block bool) (selected, received bool) {
	// raceenabled: don't need to check ep, as it is always on the stack
	// or is new memory allocated by reflect.

	if debugChan {
		print("chanrecv: chan=", c, "\n")
	}

	// 如果是一个空的channel
	if c == nil {
		if !block { //如果是非阻塞调用，直接返回（false,false）
			return
		}
		// 否则，阻塞接收，则将当前Goroutine阻塞挂起
		gopark(nil, nil, waitReasonChanReceiveNilChan, traceEvGoStop, 2)
		throw("unreachable")
	}

	// Fast path: check for failed non-blocking operation without acquiring the lock.
	// 快速检查：非阻塞模式，对于以下情况：
	// 1.无缓冲区且等待发送队列也为空
	// 2.有缓冲区但缓冲区数组为空
	if !block && empty(c) {
		// After observing that the channel is not ready for receiving, we observe whether the
		// channel is closed.
		//
		// Reordering of these checks could lead to incorrect behavior when racing with a close.
		// For example, if the channel was open and not empty, was closed, and then drained,
		// reordered reads could incorrectly indicate "open and empty". To prevent reordering,
		// we use atomic loads for both checks, and rely on emptying and closing to happen in
		// separate critical sections under the same lock.  This assumption fails when closing
		// an unbuffered channel with a blocked send, but that is an error condition anyway.
		// 如果channel未关闭，以上两种情况直接返回false，false
		if atomic.Load(&c.closed) == 0 {
			// Because a channel cannot be reopened, the later observation of the channel
			// being not closed implies that it was also not closed at the moment of the
			// first observation. We behave as if we observed the channel at that moment
			// and report that the receive cannot proceed.
			//
			// 因为channel不可以被重复打开，所以前一个观测的时候channel是未关闭的
			// 因此这种情况下可以直接宣布接收失败，返回（false, false）
			return
		}
		// The channel is irreversibly closed. Re-check whether the channel has any pending data
		// to receive, which could have arrived between the empty and closed checks above.
		// Sequential consistency is also required here, when racing with such a send.
		//
		// channel已经关闭，并且不会重新打开，再检测下channel是否还有数据
		if empty(c) {
			// The channel is irreversibly closed and empty.
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			//如果要接收数据，那就返回零值
			if ep != nil {
				// typedmemclr 根据类型清理相应地址的内存
				typedmemclr(c.elemtype, ep)
			}
			//被选中，但是没有数据
			return true, false
		}
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	lock(&c.lock)

	// 如果channel已经关闭
	if c.closed != 0 {
		//并且循环数组buf里面没有元素
		if c.qcount == 0 {
			if raceenabled {
				raceacquire(c.raceaddr())
			}
			unlock(&c.lock)
			if ep != nil {
				typedmemclr(c.elemtype, ep)
			}
			//返回选中selected=true, 没有数据
			return true, false
		}
		// The channel has been closed, but the channel's buffer have data.
	} else {
		// Just found waiting sender with not closed.
		//
		// 除了以上非常规情况，还有以下几种常见情况：
		//
		// 1.等待发送的队列sendq里存在Goroutine，那么有两种情况：当channel无缓冲区，或者当前channel buf 已满
		if sg := c.sendq.dequeue(); sg != nil {
			// Found a waiting sender. If buffer is size 0, receive value
			// directly from sender. Otherwise, receive from head of queue
			// and add sender's value to the tail of the queue (both map to
			// the same buffer slot because the queue is full).
			// 如果无缓冲区，那么直接从sender接收队数据；
			// 否则，从buf队列的头部接收数据，并把sender的数据加到buf队列的尾部
			recv(c, sg, ep, func() { unlock(&c.lock) }, 3)
			// 接收成功
			return true, true
		}
	}

	// 2. 缓冲区buf中有元素
	if c.qcount > 0 {
		// Receive directly from queue
		// 从recvx指向的位置获取元素
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
		}
		// 代码里，没有忽略要接收的值，不是 “<-ch”， 而是“val<-ch”, eq指向val
		if ep != nil {
			// 将从buf中取出的元素拷贝到当前协程
			typedmemmove(c.elemtype, ep, qp)
		}
		typedmemclr(c.elemtype, qp) //清理掉循环数组对应位置的值
		c.recvx++                   //接收游标向前移动
		if c.recvx == c.dataqsiz {  //接收游标==循环数组长度
			c.recvx = 0 //接收游标归零
		}
		c.qcount--      //buf里面的元素个数减1
		unlock(&c.lock) //解锁，返回成功
		return true, true
	}

	// 3. 非阻塞模式，且没有数据可以接受
	if !block {
		// 解锁，直接返回失败
		unlock(&c.lock)
		return false, false
	}

	// no sender available: block on this channel.
	//
	// 4.阻塞模式，获取当前Goroutine，打包一个sudog
	gp := getg()
	mysg := acquireSudog()
	mysg.releasetime = 0
	if t0 != 0 {
		mysg.releasetime = -1
	}
	// No stack splits between assigning elem and enqueuing mysg
	// on gp.waiting where copystack can find it.
	// 保存待接收数据的内存地址
	mysg.elem = ep
	mysg.waitlink = nil
	gp.waiting = mysg
	mysg.g = gp
	mysg.isSelect = false
	mysg.c = c
	gp.param = nil
	// 进入接收队列recvq中
	c.recvq.enqueue(mysg)
	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	gp.parkingOnChan.Store(true)
	gopark(chanparkcommit, unsafe.Pointer(&c.lock), waitReasonChanReceive, traceEvGoBlockRecv, 2)

	// someone woke us up
	// 被唤醒了，执行一些清理工作
	if mysg != gp.waiting {
		throw("G waiting list is corrupted")
	}
	gp.waiting = nil
	gp.activeStackChans = false
	if mysg.releasetime > 0 {
		blockevent(mysg.releasetime-t0, 2)
	}
	success := mysg.success
	gp.param = nil
	mysg.c = nil
	releaseSudog(mysg)
	return true, success
}

// recv processes a receive operation on a full channel c.
// There are 2 parts:
//  1. The value sent by the sender sg is put into the channel
//     and the sender is woken up to go on its merry way.
//  2. The value received by the receiver (the current G) is
//     written to ep.
//
// For synchronous channels, both values are the same.
// For asynchronous channels, the receiver gets its data from
// the channel buffer and the sender's data is put in the
// channel buffer.
// Channel c must be full and locked. recv unlocks c with unlockf.
// sg must already be dequeued from c.
// A non-nil ep must point to the heap or the caller's stack.
func recv(c *hchan, sg *sudog, ep unsafe.Pointer, unlockf func(), skip int) {
	if c.dataqsiz == 0 { // 如果是非缓冲channel，
		if raceenabled {
			racesync(c, sg)
		}
		if ep != nil {
			// copy data from sender
			// 直接复制数据 从 sender goroutine -> receiver goroutine
			recvDirect(c.elemtype, sg, ep)
		}
	} else {
		// Queue is full. Take the item at the
		// head of the queue. Make the sender enqueue
		// its item at the tail of the queue. Since the
		// queue is full, those are both the same slot.
		//
		// 缓存型的channel，但是buf已满
		// 将循环数组buf队首的元素直接复制到接收数据的地址
		// 将等待发送者的数据入队
		qp := chanbuf(c, c.recvx)
		if raceenabled {
			racenotify(c, c.recvx, nil)
			racenotify(c, c.recvx, sg)
		}
		// copy data from queue to receiver
		if ep != nil {
			typedmemmove(c.elemtype, ep, qp)
		}
		// copy data from sender to queue
		// 将发送者数据复制到buf
		typedmemmove(c.elemtype, qp, sg.elem)
		c.recvx++ //更新游标值
		if c.recvx == c.dataqsiz {
			c.recvx = 0
		}
		c.sendx = c.recvx // c.sendx = (c.sendx+1) % c.dataqsiz
	}
	sg.elem = nil
	gp := sg.g
	unlockf()
	gp.param = unsafe.Pointer(sg)
	sg.success = true
	if sg.releasetime != 0 {
		sg.releasetime = cputicks()
	}
	goready(gp, skip+1) //唤醒发送的goroutine，需要等待调度器
}

func chanparkcommit(gp *g, chanLock unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on the
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	gp.parkingOnChan.Store(false)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock chanLock
	// we risk gp getting readied by a channel operation and
	// so gp could continue running before everything before
	// the unlock is visible (even to gp itself).
	unlock((*mutex)(chanLock))
	return true
}

// compiler implements
//
//	select {
//	case c <- v:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selectnbsend(c, v) {
//		... foo
//	} else {
//		... bar
//	}
func selectnbsend(c *hchan, elem unsafe.Pointer) (selected bool) {
	return chansend(c, elem, false, getcallerpc())
}

// compiler implements
//
//	select {
//	case v, ok = <-c:
//		... foo
//	default:
//		... bar
//	}
//
// as
//
//	if selected, ok = selectnbrecv(&v, c); selected {
//		... foo
//	} else {
//		... bar
//	}
func selectnbrecv(elem unsafe.Pointer, c *hchan) (selected, received bool) {
	return chanrecv(c, elem, false)
}

//go:linkname reflect_chansend reflect.chansend
func reflect_chansend(c *hchan, elem unsafe.Pointer, nb bool) (selected bool) {
	return chansend(c, elem, !nb, getcallerpc())
}

//go:linkname reflect_chanrecv reflect.chanrecv
func reflect_chanrecv(c *hchan, nb bool, elem unsafe.Pointer) (selected bool, received bool) {
	return chanrecv(c, elem, !nb)
}

//go:linkname reflect_chanlen reflect.chanlen
func reflect_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflectlite_chanlen internal/reflectlite.chanlen
func reflectlite_chanlen(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.qcount)
}

//go:linkname reflect_chancap reflect.chancap
func reflect_chancap(c *hchan) int {
	if c == nil {
		return 0
	}
	return int(c.dataqsiz)
}

//go:linkname reflect_chanclose reflect.chanclose
func reflect_chanclose(c *hchan) {
	closechan(c)
}

func (q *waitq) enqueue(sgp *sudog) {
	sgp.next = nil
	x := q.last
	if x == nil {
		sgp.prev = nil
		q.first = sgp
		q.last = sgp
		return
	}
	sgp.prev = x
	x.next = sgp
	q.last = sgp
}

func (q *waitq) dequeue() *sudog {
	for {
		sgp := q.first
		if sgp == nil {
			return nil
		}
		y := sgp.next
		if y == nil {
			q.first = nil
			q.last = nil
		} else {
			y.prev = nil
			q.first = y
			sgp.next = nil // mark as removed (see dequeueSudoG)
		}

		// if a goroutine was put on this queue because of a
		// select, there is a small window between the goroutine
		// being woken up by a different case and it grabbing the
		// channel locks. Once it has the lock
		// it removes itself from the queue, so we won't see it after that.
		// We use a flag in the G struct to tell us when someone
		// else has won the race to signal this goroutine but the goroutine
		// hasn't removed itself from the queue yet.
		if sgp.isSelect && !sgp.g.selectDone.CompareAndSwap(0, 1) {
			continue
		}

		return sgp
	}
}

func (c *hchan) raceaddr() unsafe.Pointer {
	// Treat read-like and write-like operations on the channel to
	// happen at this address. Avoid using the address of qcount
	// or dataqsiz, because the len() and cap() builtins read
	// those addresses, and we don't want them racing with
	// operations like close().
	return unsafe.Pointer(&c.buf)
}

func racesync(c *hchan, sg *sudog) {
	racerelease(chanbuf(c, 0))
	raceacquireg(sg.g, chanbuf(c, 0))
	racereleaseg(sg.g, chanbuf(c, 0))
	raceacquire(chanbuf(c, 0))
}

// Notify the race detector of a send or receive involving buffer entry idx
// and a channel c or its communicating partner sg.
// This function handles the special case of c.elemsize==0.
func racenotify(c *hchan, idx uint, sg *sudog) {
	// We could have passed the unsafe.Pointer corresponding to entry idx
	// instead of idx itself.  However, in a future version of this function,
	// we can use idx to better handle the case of elemsize==0.
	// A future improvement to the detector is to call TSan with c and idx:
	// this way, Go will continue to not allocating buffer entries for channels
	// of elemsize==0, yet the race detector can be made to handle multiple
	// sync objects underneath the hood (one sync object per idx)
	qp := chanbuf(c, idx)
	// When elemsize==0, we don't allocate a full buffer for the channel.
	// Instead of individual buffer entries, the race detector uses the
	// c.buf as the only buffer entry.  This simplification prevents us from
	// following the memory model's happens-before rules (rules that are
	// implemented in racereleaseacquire).  Instead, we accumulate happens-before
	// information in the synchronization object associated with c.buf.
	if c.elemsize == 0 {
		if sg == nil {
			raceacquire(qp)
			racerelease(qp)
		} else {
			raceacquireg(sg.g, qp)
			racereleaseg(sg.g, qp)
		}
	} else {
		if sg == nil {
			racereleaseacquire(qp)
		} else {
			racereleaseacquireg(sg.g, qp)
		}
	}
}
