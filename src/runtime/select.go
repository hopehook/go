// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

// This file contains the implementation of Go select statements.

import (
	"internal/abi"
	"runtime/internal/atomic"
	"unsafe"
)

const debugSelect = false

// Select case descriptor.
// Known to compiler.
// Changes here must also be made in src/cmd/compile/internal/walk/select.go's scasetype.
type scase struct {
	c    *hchan         // chan
	elem unsafe.Pointer // data element
}

var (
	chansendpc = abi.FuncPCABIInternal(chansend)
	chanrecvpc = abi.FuncPCABIInternal(chanrecv)
)

func selectsetpc(pc *uintptr) {
	*pc = getcallerpc()
}

func sellock(scases []scase, lockorder []uint16) {
	var c *hchan
	for _, o := range lockorder {
		c0 := scases[o].c // 根据加锁顺序获取 case

		// c 记录了上次加锁的 hchan 地址，如果和当前 *hchan 相同，那么就不会再次加锁
		if c0 != c {
			c = c0
			lock(&c.lock)
		}
	}
}

func selunlock(scases []scase, lockorder []uint16) {
	// We must be very careful here to not touch sel after we have unlocked
	// the last lock, because sel can be freed right after the last unlock.
	// Consider the following situation.
	// First M calls runtime·park() in runtime·selectgo() passing the sel.
	// Once runtime·park() has unlocked the last lock, another M makes
	// the G that calls select runnable again and schedules it for execution.
	// When the G runs on another M, it locks all the locks and frees sel.
	// Now if the first M touches sel, it will access freed memory.
	for i := len(lockorder) - 1; i >= 0; i-- {
		c := scases[lockorder[i]].c
		if i > 0 && c == scases[lockorder[i-1]].c {
			continue // will unlock it on the next iteration
		}
		unlock(&c.lock)
	}
}

func selparkcommit(gp *g, _ unsafe.Pointer) bool {
	// There are unlocked sudogs that point into gp's stack. Stack
	// copying must lock the channels of those sudogs.
	// Set activeStackChans here instead of before we try parking
	// because we could self-deadlock in stack growth on a
	// channel lock.
	gp.activeStackChans = true
	// Mark that it's safe for stack shrinking to occur now,
	// because any thread acquiring this G's stack for shrinking
	// is guaranteed to observe activeStackChans after this store.
	atomic.Store8(&gp.parkingOnChan, 0)
	// Make sure we unlock after setting activeStackChans and
	// unsetting parkingOnChan. The moment we unlock any of the
	// channel locks we risk gp getting readied by a channel operation
	// and so gp could continue running before everything before the
	// unlock is visible (even to gp itself).

	// This must not access gp's stack (see gopark). In
	// particular, it must not access the *hselect. That's okay,
	// because by the time this is called, gp.waiting has all
	// channels in lock order.
	var lastc *hchan
	for sg := gp.waiting; sg != nil; sg = sg.waitlink {
		if sg.c != lastc && lastc != nil {
			// As soon as we unlock the channel, fields in
			// any sudog with that channel may change,
			// including c and waitlink. Since multiple
			// sudogs may have the same channel, we unlock
			// only after we've passed the last instance
			// of a channel.
			unlock(&lastc.lock)
		}
		lastc = sg.c
	}
	if lastc != nil {
		unlock(&lastc.lock)
	}
	return true
}

// 空的 select 语句会被转换成调用 runtime.block 直接挂起当前 Goroutine；
func block() {
	gopark(nil, nil, waitReasonSelectNoCases, traceEvGoStop, 1) // forever
}

// selectgo implements the select statement.
//
// cas0 points to an array of type [ncases]scase, and order0 points to
// an array of type [2*ncases]uint16 where ncases must be <= 65536.
// Both reside on the goroutine's stack (regardless of any escaping in
// selectgo).
//
// For race detector builds, pc0 points to an array of type
// [ncases]uintptr (also on the stack); for other builds, it's set to
// nil.
//
// selectgo returns the index of the chosen scase, which matches the
// ordinal position of its respective select{recv,send,default} call.
// Also, if the chosen scase was a receive operation, it reports whether
// a value was received.
//
// 在编译器已经对 select 语句进行优化之后，Go 语言会在运行时执行编译期间展开的 runtime.selectgo 函数，该函数会按照以下的流程执行：
//   1. 随机生成一个遍历的轮询顺序 pollOrder 并根据 Channel 地址生成锁定顺序 lockOrder；
//   2. 根据 pollOrder 遍历所有的 case 查看是否有可以立刻处理的 Channel；
//      如果存在，直接获取 case 对应的索引并返回；
//      如果不存在，创建 runtime.sudog 结构体，将当前 Goroutine 加入到所有相关 Channel 的收发队列，并调用 runtime.gopark 挂起当前 Goroutine 等待调度器的唤醒；
//   3. 当调度器唤醒当前 Goroutine 时，会再次按照 lockOrder 遍历所有的 case，从中查找需要被处理的 runtime.sudog 对应的索引；
//
// select 关键字是 Go 语言特有的控制结构，它的实现原理比较复杂，需要编译器和运行时函数的通力合作。
// 一个包含三个 case 的正常 select 语句其实会被展开成如下所示的逻辑，我们可以看到其中处理的三个部分：
//	selv := [3]scase{}
//	order := [6]uint16
//	for i, cas := range cases {
//		c := scase{}
//		c.kind = ...
//		c.elem = ...
//		c.c = ...
//	}
//	chosen, revcOK := selectgo(selv, order, 3)
//	if chosen == 0 {
//		...
//		break
//	}
//	if chosen == 1 {
//		...
//		break
//	}
//	if chosen == 2 {
//		...
//		break
//	}
//
//
// selectgo 参数:
//   cas0 指向一个类型为 [ncases]scase 的数组
//   order0 是一个指向[2*ncases]uint16, 数组中的值都是 0
// selectgo 会返回选中的序号，如果是个接收操作，还会返回是否接收到一个值
func selectgo(cas0 *scase, order0 *uint16, pc0 *uintptr, nsends, nrecvs int, block bool) (int, bool) {
	if debugSelect {
		print("select: cas0=", cas0, "\n")
	}

	// NOTE: In order to maintain a lean stack size, the number of scases
	// is capped at 65536.
	cas1 := (*[1 << 16]scase)(unsafe.Pointer(cas0))
	order1 := (*[1 << 17]uint16)(unsafe.Pointer(order0))

	ncases := nsends + nrecvs

	// [:n:n] 的方式会让 slice 的 len 和 cap 相等
	scases := cas1[:ncases:ncases]
	pollorder := order1[:ncases:ncases]
	lockorder := order1[ncases:][:ncases:ncases]
	// NOTE: pollorder/lockorder's underlying array was not zero-initialized by compiler.

	// Even when raceenabled is true, there might be select
	// statements in packages compiled without -race (e.g.,
	// ensureSigM in runtime/signal_unix.go).
	var pcs []uintptr
	if raceenabled && pc0 != nil {
		pc1 := (*[1 << 16]uintptr)(unsafe.Pointer(pc0))
		pcs = pc1[:ncases:ncases]
	}
	casePC := func(casi int) uintptr {
		if pcs == nil {
			return 0
		}
		return pcs[casi]
	}

	var t0 int64
	if blockprofilerate > 0 {
		t0 = cputicks()
	}

	// The compiler rewrites selects that statically have
	// only 0 or 1 cases plus default into simpler constructs.
	// The only way we can end up with such small sel.ncase
	// values here is for a larger select in which most channels
	// have been nilled out. The general code handles those
	// cases correctly, and they are rare enough not to bother
	// optimizing (and needing to test).

	// generate permuted order
	//
	// pollorder 在开始的时候值都是 0，循环结束后值便是随机顺序的 scases 索引
	norder := 0
	for i := range scases {
		cas := &scases[i]

		// Omit cases without channels from the poll and lock orders.
		// 为 nil 的 channel 直接忽略掉
		if cas.c == nil {
			cas.elem = nil // allow GC
			continue
		}

		j := fastrandn(uint32(norder + 1))
		pollorder[norder] = pollorder[j]
		pollorder[j] = uint16(i)
		norder++
	}

	// 轮询顺序：通过 runtime.fastrandn 函数引入随机性；
	//  - 避免饥饿：随机的轮询顺序可以避免 Channel 的饥饿问题，保证公平性；
	pollorder = pollorder[:norder]
	// 加锁顺序：按照 Channel 的地址排序后确定加锁顺序；
	//  - 避免死锁：根据 Channel 的地址顺序确定加锁顺序能够避免死锁的发生。
	lockorder = lockorder[:norder]

	// sort the cases by Hchan address to get the locking order.
	// simple heap sort, to guarantee n log n time and constant stack footprint.
	for i := range lockorder {
		j := i
		// Start with the pollorder to permute cases on the same channel.
		c := scases[pollorder[i]].c
		for j > 0 && scases[lockorder[(j-1)/2]].c.sortkey() < c.sortkey() {
			k := (j - 1) / 2
			lockorder[j] = lockorder[k]
			j = k
		}
		lockorder[j] = pollorder[i]
	}
	for i := len(lockorder) - 1; i >= 0; i-- {
		o := lockorder[i]
		c := scases[o].c
		lockorder[i] = lockorder[0]
		j := 0
		for {
			k := j*2 + 1
			if k >= i {
				break
			}
			if k+1 < i && scases[lockorder[k]].c.sortkey() < scases[lockorder[k+1]].c.sortkey() {
				k++
			}
			if c.sortkey() < scases[lockorder[k]].c.sortkey() {
				lockorder[j] = lockorder[k]
				j = k
				continue
			}
			break
		}
		lockorder[j] = o
	}

	if debugSelect {
		for i := 0; i+1 < len(lockorder); i++ {
			if scases[lockorder[i]].c.sortkey() > scases[lockorder[i+1]].c.sortkey() {
				print("i=", i, " x=", lockorder[i], " y=", lockorder[i+1], "\n")
				throw("select: broken sort")
			}
		}
	}

	// lock all the channels involved in the select
	// 根据 Channel 的地址排序确定加锁顺序
	// sellock 对地址相同的 channel 只会加锁一次
	sellock(scases, lockorder)

	var (
		gp     *g
		sg     *sudog
		c      *hchan
		k      *scase
		sglist *sudog
		sgnext *sudog
		qp     unsafe.Pointer
		nextp  **sudog
	)

	// pass 1 - look for something already waiting
	// 循环执行的第一个阶段，查找已经准备就绪的 Channel。
	//
	// 第一阶段的主要职责是查找所有 case 中是否有可以立刻被处理的 Channel。
	// 无论是在等待的 Goroutine 上，还是缓冲区中，只要存在数据满足条件就会立刻处理，
	// 如果不能立刻找到活跃的 Channel 就会进入循环的下一阶段，
	// 按照需要将当前 Goroutine 加入到 Channel 的 sendq 或者 recvq 队列中
	//
	//
	// runtime.selectgo 函数会根据不同情况通过 goto 语句跳转到函数内部的不同标签执行相应的逻辑，其中包括：
	//	 bufrecv：可以从缓冲区读取数据；
	//	 bufsend：可以向缓冲区写入数据；
	//	 recv：可以从休眠的发送方获取数据；
	//	 send：可以向休眠的接收方发送数据；
	//	 rclose：可以从关闭的 Channel 读取 EOF；
	//	 sclose：向关闭的 Channel 发送数据；
	//	 retc：结束调用并返回；
	var casi int
	var cas *scase
	var caseSuccess bool
	var caseReleaseTime int64 = -1
	var recvOK bool
	for _, casei := range pollorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c

		if casi >= nsends {
			// 如果 channel 中有待发送的 goroutine, 跳转到 recv，调用 recv 完成接收操作
			sg = c.sendq.dequeue()
			if sg != nil {
				goto recv
			}

			// 如果 channel 中有缓冲数据，那么跳转到 bufrecv，从缓冲区中获取数据
			if c.qcount > 0 {
				goto bufrecv
			}

			// 如果 channel 已关闭，跳转到 rclose, 将接收值置为空值，recvOK 置为 false
			if c.closed != 0 {
				goto rclose
			}
		} else {
			if raceenabled {
				racereadpc(c.raceaddr(), casePC(casi), chansendpc)
			}

			// 对于发送操作会先判断 channel 是否已经关闭，跳转到 sclose，直接 panic
			if c.closed != 0 {
				goto sclose
			}

			// 如果 channel 未关闭，并且有待接收队列不为空
			// 说明 channel 的缓冲区为空，跳转到 send , 调用 send 函数，直接发送数据给待接收者
			sg = c.recvq.dequeue()
			if sg != nil {
				goto send
			}

			// 如果缓冲区不为空的话，跳转到 bufsend，从缓冲区获取数据
			if c.qcount < c.dataqsiz {
				goto bufsend
			}
		}
	}

	// 如果不是阻塞的 select (有 default 分支)，获取数据失败，直接结束
	if !block {
		selunlock(scases, lockorder)
		casi = -1
		goto retc
	}

	// pass 2 - enqueue on all chans
	// 如果没有 channel 可以执行收发操作，并且没有 default case，
	// 那么就将当前 goroutine 加入到 channel 相应的收发队列中，等待被其他 goroutine 唤醒
	gp = getg()
	if gp.waiting != nil {
		throw("gp.waiting != nil")
	}

	// 这些 sudog 结构体都会被串成链表附着在当前 Goroutine 上
	nextp = &gp.waiting

	// 在入队之后会调用 gopark 函数挂起当前的 Goroutine 等待调度器的唤醒。
	for _, casei := range lockorder {
		casi = int(casei)
		cas = &scases[casi]
		c = cas.c

		// 构造 sudog
		sg := acquireSudog()
		sg.g = gp
		sg.isSelect = true
		// No stack splits between assigning elem and enqueuing
		// sg on gp.waiting where copystack can find it.
		sg.elem = cas.elem
		sg.releasetime = 0
		if t0 != 0 {
			sg.releasetime = -1
		}
		sg.c = c
		// Construct waiting list in lock order.
		*nextp = sg
		nextp = &sg.waitlink

		// 加入相应等待队列
		if casi < nsends {
			c.sendq.enqueue(sg)
		} else {
			c.recvq.enqueue(sg)
		}
	}

	// wait for someone to wake us up
	// 被唤醒后会根据 param 来判断是否是由 close 操作唤醒的，所以先置为 nil
	gp.param = nil

	// Signal to anyone trying to shrink our stack that we're about
	// to park on a channel. The window between when this G's status
	// changes and when we set gp.activeStackChans is not safe for
	// stack shrinking.
	atomic.Store8(&gp.parkingOnChan, 1)

	// channel send/recv dequeue: 这里只会有 1 个 goroutine 被唤醒 (goready)
	// 因为 dequeue 函数里 CAS 操作 goroutine.selectDone 的存在
	gopark(selparkcommit, nil, waitReasonSelect, traceEvGoBlockSelect, 1)

	// 这里已经被唤醒了，接下来选择合适的 case
	gp.activeStackChans = false

	// 加锁所有的 channel
	sellock(scases, lockorder)

	gp.selectDone = 0

	// param 存放唤醒 goroutine 的 sudog，如果是关闭操作唤醒的，那么就为 nil
	sg = (*sudog)(gp.param)
	gp.param = nil

	// pass 3 - dequeue from unsuccessful chans
	// otherwise they stack up on quiet channels
	// record the successful case, if any.
	// We singly-linked up the SudoGs in lock order.
	//
	// 第三次遍历全部 case 时，我们会先获取当前 Goroutine 接收到的参数 sudog 结构，
	// 我们会依次对比所有 case 对应的 sudog 结构找到被唤醒的 case，获取该 case 对应的索引并返回。
	//
	// 由于当前的 select 结构找到了一个 case 执行，那么剩下 case 中没有被用到的 sudog 就会被忽略并且释放掉。
	// 为了不影响 Channel 的正常使用，我们还是需要将这些废弃的 sudog 从 Channel 中出队 (dequeueSudoG)。
	// 注意：
	//	- dequeue: 这里只会有 1 个 goroutine 被唤醒 (goready)，因为 CAS 操作 goroutine.selectDone 的存在
	//  - dequeueSudoG: 这个函数只是释放 SudoG 的，select 特有。
	casi = -1
	cas = nil // cas 便是唤醒 goroutine 的 case
	caseSuccess = false

	// waiting 链表按照 lockorder 顺序存放着 sudog
	sglist = gp.waiting

	// Clear all elem before unlinking from gp.waiting.
	for sg1 := gp.waiting; sg1 != nil; sg1 = sg1.waitlink {
		sg1.isSelect = false
		sg1.elem = nil
		sg1.c = nil
	}
	gp.waiting = nil

	// 当前 goroutine 被唤醒后，将其他 sudog 从相应的 channel 等待队列中移除
	for _, casei := range lockorder {
		k = &scases[casei]

		// 如果相等说明，goroutine 是被当前 case 的 channel 收发操作唤醒的
		// 如果是关闭操作，那么 sg 为 nil, 不会对 cas 赋值
		if sg == sglist {
			// sg has already been dequeued by the G that woke us up.
			casi = int(casei)
			cas = k
			caseSuccess = sglist.success
			if sglist.releasetime > 0 {
				caseReleaseTime = sglist.releasetime
			}
		} else {
			// goroutine 已经被唤醒，将 sudog 从相应的收发队列中移除
			c = k.c

			// func (q *waitq) dequeueSudoG(sgp *sudog)
			// dequeueSudoG 会通过 sudog.prev 和 sudog.next 将 sudog 从等待队列中移除
			if int(casei) < nsends {
				c.sendq.dequeueSudoG(sglist)
			} else {
				c.recvq.dequeueSudoG(sglist)
			}
		}

		// 释放 sudog，然后准备处理下一个 sudog
		sgnext = sglist.waitlink
		sglist.waitlink = nil
		releaseSudog(sglist)
		sglist = sgnext
	}

	// 由关闭操作唤醒 goroutine，现在的代码应该不存在这种情况了
	if cas == nil {
		throw("selectgo: bad wakeup")
	}

	c = cas.c

	if debugSelect {
		print("wait-return: cas0=", cas0, " c=", c, " cas=", cas, " send=", casi < nsends, "\n")
	}

	if casi < nsends {
		if !caseSuccess {
			goto sclose
		}
	} else {
		recvOK = caseSuccess
	}

	if raceenabled {
		if casi < nsends {
			raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
		} else if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
	}
	if msanenabled {
		if casi < nsends {
			msanread(cas.elem, c.elemtype.size)
		} else if cas.elem != nil {
			msanwrite(cas.elem, c.elemtype.size)
		}
	}
	if asanenabled {
		if casi < nsends {
			asanread(cas.elem, c.elemtype.size)
		} else if cas.elem != nil {
			asanwrite(cas.elem, c.elemtype.size)
		}
	}

	selunlock(scases, lockorder)
	goto retc

bufrecv:
	// can receive from buffer
	if raceenabled {
		if cas.elem != nil {
			raceWriteObjectPC(c.elemtype, cas.elem, casePC(casi), chanrecvpc)
		}
		racenotify(c, c.recvx, nil)
	}
	if msanenabled && cas.elem != nil {
		msanwrite(cas.elem, c.elemtype.size)
	}
	if asanenabled && cas.elem != nil {
		asanwrite(cas.elem, c.elemtype.size)
	}
	recvOK = true
	qp = chanbuf(c, c.recvx)
	if cas.elem != nil {
		typedmemmove(c.elemtype, cas.elem, qp)
	}
	typedmemclr(c.elemtype, qp)
	c.recvx++
	if c.recvx == c.dataqsiz {
		c.recvx = 0
	}
	c.qcount--
	selunlock(scases, lockorder)
	goto retc

bufsend:
	// can send to buffer
	if raceenabled {
		racenotify(c, c.sendx, nil)
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.size)
	}
	typedmemmove(c.elemtype, chanbuf(c, c.sendx), cas.elem)
	c.sendx++
	if c.sendx == c.dataqsiz {
		c.sendx = 0
	}
	c.qcount++
	selunlock(scases, lockorder)
	goto retc

recv:
	// can receive from sleeping sender (sg)
	recv(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncrecv: cas0=", cas0, " c=", c, "\n")
	}
	recvOK = true
	goto retc

rclose:
	// read at end of closed channel
	selunlock(scases, lockorder)
	recvOK = false
	if cas.elem != nil {
		typedmemclr(c.elemtype, cas.elem)
	}
	if raceenabled {
		raceacquire(c.raceaddr())
	}
	goto retc

send:
	// can send to a sleeping receiver (sg)
	if raceenabled {
		raceReadObjectPC(c.elemtype, cas.elem, casePC(casi), chansendpc)
	}
	if msanenabled {
		msanread(cas.elem, c.elemtype.size)
	}
	if asanenabled {
		asanread(cas.elem, c.elemtype.size)
	}
	send(c, sg, cas.elem, func() { selunlock(scases, lockorder) }, 2)
	if debugSelect {
		print("syncsend: cas0=", cas0, " c=", c, "\n")
	}
	goto retc

retc:
	if caseReleaseTime > 0 {
		blockevent(caseReleaseTime-t0, 1)
	}
	return casi, recvOK

sclose:
	// send on closed channel
	selunlock(scases, lockorder)
	panic(plainError("send on closed channel"))
}

func (c *hchan) sortkey() uintptr {
	return uintptr(unsafe.Pointer(c))
}

// A runtimeSelect is a single case passed to rselect.
// This must match ../reflect/value.go:/runtimeSelect
type runtimeSelect struct {
	dir selectDir
	typ unsafe.Pointer // channel type (not used here)
	ch  *hchan         // channel
	val unsafe.Pointer // ptr to data (SendDir) or ptr to receive buffer (RecvDir)
}

// These values must match ../reflect/value.go:/SelectDir.
type selectDir int

const (
	_             selectDir = iota
	selectSend              // case Chan <- Send
	selectRecv              // case <-Chan:
	selectDefault           // default
)

//go:linkname reflect_rselect reflect.rselect
func reflect_rselect(cases []runtimeSelect) (int, bool) {
	if len(cases) == 0 {
		block()
	}
	sel := make([]scase, len(cases))
	orig := make([]int, len(cases))
	nsends, nrecvs := 0, 0
	dflt := -1
	for i, rc := range cases {
		var j int
		switch rc.dir {
		case selectDefault:
			dflt = i
			continue
		case selectSend:
			j = nsends
			nsends++
		case selectRecv:
			nrecvs++
			j = len(cases) - nrecvs
		}

		sel[j] = scase{c: rc.ch, elem: rc.val}
		orig[j] = i
	}

	// Only a default case.
	if nsends+nrecvs == 0 {
		return dflt, false
	}

	// Compact sel and orig if necessary.
	if nsends+nrecvs < len(cases) {
		copy(sel[nsends:], sel[len(cases)-nrecvs:])
		copy(orig[nsends:], orig[len(cases)-nrecvs:])
	}

	order := make([]uint16, 2*(nsends+nrecvs))
	var pc0 *uintptr
	if raceenabled {
		pcs := make([]uintptr, nsends+nrecvs)
		for i := range pcs {
			selectsetpc(&pcs[i])
		}
		pc0 = &pcs[0]
	}

	chosen, recvOK := selectgo(&sel[0], &order[0], pc0, nsends, nrecvs, dflt == -1)

	// Translate chosen back to caller's ordering.
	if chosen < 0 {
		chosen = dflt
	} else {
		chosen = orig[chosen]
	}
	return chosen, recvOK
}

func (q *waitq) dequeueSudoG(sgp *sudog) {
	x := sgp.prev
	y := sgp.next
	if x != nil {
		if y != nil {
			// middle of queue
			x.next = y
			y.prev = x
			sgp.next = nil
			sgp.prev = nil
			return
		}
		// end of queue
		x.next = nil
		q.last = x
		sgp.prev = nil
		return
	}
	if y != nil {
		// start of queue
		y.prev = nil
		q.first = y
		sgp.next = nil
		return
	}

	// x==y==nil. Either sgp is the only element in the queue,
	// or it has already been removed. Use q.first to disambiguate.
	if q.first == sgp {
		q.first = nil
		q.last = nil
	}
}
