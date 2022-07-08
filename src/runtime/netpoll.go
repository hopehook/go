// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build aix || darwin || dragonfly || freebsd || (js && wasm) || linux || netbsd || openbsd || solaris || windows

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// Integrated network poller (platform-independent part).
// A particular implementation (epoll/kqueue/port/AIX/Windows)
// must define the following functions:
//
// func netpollinit()
//     Initialize the poller. Only called once.
//
// func netpollopen(fd uintptr, pd *pollDesc) int32
//     Arm edge-triggered notifications for fd. The pd argument is to pass
//     back to netpollready when fd is ready. Return an errno value.
//
// func netpollclose(fd uintptr) int32
//     Disable notifications for fd. Return an errno value.
//
// func netpoll(delta int64) gList
//     Poll the network. If delta < 0, block indefinitely. If delta == 0,
//     poll without blocking. If delta > 0, block for up to delta nanoseconds.
//     Return a list of goroutines built by calling netpollready.
//
// func netpollBreak()
//     Wake up the network poller, assumed to be blocked in netpoll.
//
// func netpollIsPollDescriptor(fd uintptr) bool
//     Reports whether fd is a file descriptor used by the poller.

// Error codes returned by runtime_pollReset and runtime_pollWait.
// These must match the values in internal/poll/fd_poll_runtime.go.
const (
	pollNoError        = 0 // no error
	pollErrClosing     = 1 // descriptor is closed
	pollErrTimeout     = 2 // I/O timeout
	pollErrNotPollable = 3 // general error polling descriptor
)

// pollDesc contains 2 binary semaphores, rg and wg, to park reader and writer
// goroutines respectively. The semaphore can be in the following states:
// pdReady - io readiness notification is pending;
//
//	a goroutine consumes the notification by changing the state to nil.
//
// pdWait - a goroutine prepares to park on the semaphore, but not yet parked;
//
//	the goroutine commits to park by changing the state to G pointer,
//	or, alternatively, concurrent io notification changes the state to pdReady,
//	or, alternatively, concurrent timeout/close changes the state to nil.
//
// G pointer - the goroutine is blocked on the semaphore;
//
//	io notification or timeout/close changes the state to pdReady or nil respectively
//	and unparks the goroutine.
//
// nil - none of the above.
const (
	pdReady uintptr = 1
	pdWait  uintptr = 2
)

const pollBlockSize = 4 * 1024

// Network poller descriptor.
//
// No heap pointers.
//

// 该结构体中包含用于监控可读和可写状态的变量，我们按照功能将它们分成以下四组：
// rseq 和 wseq  — 表示文件描述符被重用或者计时器被重置；
// rg 和 wg      — 表示二进制的信号量，可能为 pdReady、pdWait、等待文件描述符可读或者可写的 Goroutine 以及 nil；
// rd 和 wd      — 等待文件描述符可读或者可写的截止日期；
// rt 和 wt      — 用于等待文件描述符的计时器；
//
// pollDesc 结构体中的 rg 和 wg 比较难理解，它们与 netpoll 相关，将底层缓存区的读写情况反映为当前读写对应的 goroutine 的状态。
// 当读缓存区没有数据时，会导致 rg 阻塞(非 pdReady)，此时调用 netpollunblock 返回的为读操作所在的 goroutine；
// 而当执行 write 操作时，如果缓存区没有空间，此时会导致 wg 阻塞，此时调用 netpollunblock 返回的为写操作所在的 goroutine。
// 在非阻塞时，rg/wg 表示当前读/写 goroutine 状态，pdReady 表示可以进行读/写操作，pdWait 表示当前 goroutine 将会被 park(调用 gopark)住。
// *注意：用户读写操作可以使用同一个 goroutine。
//go:notinheap
type pollDesc struct {
	link *pollDesc // in pollcache, protected by pollcache.lock

	// The lock protects pollOpen, pollSetDeadline, pollUnblock and deadlineimpl operations.
	// This fully covers seq, rt and wt variables. fd is constant throughout the PollDesc lifetime.
	// pollReset, pollWait, pollWaitCanceled and runtime·netpollready (IO readiness notification)
	// proceed w/o taking the lock. So closing, everr, rg, rd, wg and wd are manipulated
	// in a lock-free way by all operations.
	// TODO(golang.org/issue/49008): audit these lock-free fields for continued correctness.
	// NOTE(dvyukov): the following code uses uintptr to store *g (rg/wg),
	// that will blow up when GC starts moving objects.
	lock    mutex // protects the following fields
	fd      uintptr
	closing bool
	everr   bool      // marks event scanning error happened
	user    uint32    // user settable cookie
	rseq    uintptr   // protects from stale read timers
	rg      uintptr   // pdReady, pdWait, G waiting for read or nil. Accessed atomically.
	rt      timer     // read deadline timer (set if rt.f != nil)
	rd      int64     // read deadline
	wseq    uintptr   // protects from stale write timers
	wg      uintptr   // pdReady, pdWait, G waiting for write or nil. Accessed atomically.
	wt      timer     // write deadline timer
	wd      int64     // write deadline
	self    *pollDesc // storage for indirect interface. See (*pollDesc).makeArg.
}

type pollCache struct {
	lock  mutex
	first *pollDesc
	// PollDesc objects must be type-stable,
	// because we can get ready notification from epoll/kqueue
	// after the descriptor is closed/reused.
	// Stale notifications are detected using seq variable,
	// seq is incremented when deadlines are changed or descriptor is reused.
}

var (
	netpollInitLock mutex
	netpollInited   uint32

	pollcache      pollCache
	netpollWaiters uint32
)

//go:linkname poll_runtime_pollServerInit internal/poll.runtime_pollServerInit
func poll_runtime_pollServerInit() {
	netpollGenericInit()
}

func netpollGenericInit() {
	if atomic.Load(&netpollInited) == 0 {
		lockInit(&netpollInitLock, lockRankNetpollInit)
		lock(&netpollInitLock)
		if netpollInited == 0 {
			netpollinit()
			// 调用 atomic.Store 将 netpollInited 设置为1，表示已经初始化 epoll 文件句柄。
			atomic.Store(&netpollInited, 1)
		}
		unlock(&netpollInitLock)
	}
}

func netpollinited() bool {
	return atomic.Load(&netpollInited) != 0
}

//go:linkname poll_runtime_isPollServerDescriptor internal/poll.runtime_isPollServerDescriptor

// poll_runtime_isPollServerDescriptor reports whether fd is a
// descriptor being used by netpoll.
func poll_runtime_isPollServerDescriptor(fd uintptr) bool {
	return netpollIsPollDescriptor(fd)
}

//go:linkname poll_runtime_pollOpen internal/poll.runtime_pollOpen
func poll_runtime_pollOpen(fd uintptr) (*pollDesc, int) {
	// 获取一个 pollDesc。pollcache 为全局 pd 链表，可以为 listen 和 accept 过程提供 pd。
	pd := pollcache.alloc()
	lock(&pd.lock)

	// pd 全局链表中的节点都应该是可用或初始状态，0 为初始化状态
	wg := atomic.Loaduintptr(&pd.wg)
	if wg != 0 && wg != pdReady {
		throw("runtime: blocked write on free polldesc")
	}
	rg := atomic.Loaduintptr(&pd.rg)
	if rg != 0 && rg != pdReady {
		throw("runtime: blocked read on free polldesc")
	}

	// pollDesc 中保存了文件描述符
	pd.fd = fd
	// 此处将pd的状态初始化为 false，表示该pd可用
	pd.closing = false
	pd.everr = false
	pd.rseq++
	atomic.Storeuintptr(&pd.rg, 0)
	pd.rd = 0
	pd.wseq++
	atomic.Storeuintptr(&pd.wg, 0)
	pd.wd = 0
	pd.self = pd
	unlock(&pd.lock)

	// 注册文件描述符到 epoll 句柄中
	errno := netpollopen(fd, pd)
	if errno != 0 {
		pollcache.free(pd)
		return nil, int(errno)
	}
	return pd, 0
}

//go:linkname poll_runtime_pollClose internal/poll.runtime_pollClose
func poll_runtime_pollClose(pd *pollDesc) {
	if !pd.closing {
		throw("runtime: close polldesc w/o unblock")
	}

	wg := atomic.Loaduintptr(&pd.wg)
	// 执行本函数前需要调用 netpollunblock 将 goroutine 变为非阻塞状态,以便回收 pd 节点
	if wg != 0 && wg != pdReady {
		throw("runtime: blocked write on closing polldesc")
	}
	rg := atomic.Loaduintptr(&pd.rg)
	if rg != 0 && rg != pdReady {
		throw("runtime: blocked read on closing polldesc")
	}

	// 从全局的 epfd 中删除待监听的文件描述符
	netpollclose(pd.fd)

	// 回收 fd 节点
	pollcache.free(pd)
}

func (c *pollCache) free(pd *pollDesc) {
	lock(&c.lock)
	pd.link = c.first
	c.first = pd
	unlock(&c.lock)
}

// poll_runtime_pollReset, which is internal/poll.runtime_pollReset,
// prepares a descriptor for polling in mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//
//go:linkname poll_runtime_pollReset internal/poll.runtime_pollReset
func poll_runtime_pollReset(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}
	if mode == 'r' {
		atomic.Storeuintptr(&pd.rg, 0)
	} else if mode == 'w' {
		atomic.Storeuintptr(&pd.wg, 0)
	}
	return pollNoError
}

// poll_runtime_pollWait, which is internal/poll.runtime_pollWait,
// waits for a descriptor to be ready for reading or writing,
// according to mode, which is 'r' or 'w'.
// This returns an error code; the codes are defined above.
//
//go:linkname poll_runtime_pollWait internal/poll.runtime_pollWait
func poll_runtime_pollWait(pd *pollDesc, mode int) int {
	errcode := netpollcheckerr(pd, int32(mode))
	if errcode != pollNoError {
		return errcode
	}
	// As for now only Solaris, illumos, and AIX use level-triggered IO.
	if GOOS == "solaris" || GOOS == "illumos" || GOOS == "aix" {
		netpollarm(pd, mode)
	}
	// 进入 netpollblock 并且判断是否有期待的 I/O 事件发生，
	// 这里的 for 循环是为了一直等到 io ready
	for !netpollblock(pd, int32(mode), false) {
		errcode = netpollcheckerr(pd, int32(mode))
		if errcode != pollNoError {
			return errcode
		}
		// Can happen if timeout has fired and unblocked us,
		// but before we had a chance to run, timeout has been reset.
		// Pretend it has not happened and retry.
	}
	return pollNoError
}

//go:linkname poll_runtime_pollWaitCanceled internal/poll.runtime_pollWaitCanceled
func poll_runtime_pollWaitCanceled(pd *pollDesc, mode int) {
	// This function is used only on windows after a failed attempt to cancel
	// a pending async IO operation. Wait for ioready, ignore closing or timeouts.
	for !netpollblock(pd, int32(mode), true) {
	}
}

//go:linkname poll_runtime_pollSetDeadline internal/poll.runtime_pollSetDeadline
func poll_runtime_pollSetDeadline(pd *pollDesc, d int64, mode int) {
	lock(&pd.lock)
	if pd.closing {
		unlock(&pd.lock)
		return
	}
	rd0, wd0 := pd.rd, pd.wd
	combo0 := rd0 > 0 && rd0 == wd0
	if d > 0 {
		// 获取 deadline 时间
		d += nanotime()
		// 从注释看，这种情况表示 deadline 时间小于等于当前时间。将 deadline 时间设置为 64 bit 的最大值
		if d <= 0 {
			// If the user has a deadline in the future, but the delay calculation
			// overflows, then set the deadline to the maximum possible value.
			d = 1<<63 - 1
		}
	}

	// 按照不同 mode 设置读写对应的 deadline 时间
	if mode == 'r' || mode == 'r'+'w' {
		pd.rd = d
	}
	if mode == 'w' || mode == 'r'+'w' {
		pd.wd = d
	}

	// combo 用于表示仅设置了读 deadline 还是同时设置了读写 deadline
	combo := pd.rd > 0 && pd.rd == pd.wd

	// 读场景下 deadline 时间点执行的函数
	rtf := netpollReadDeadline

	// 读场景下 deadline 时间点执行的函数
	if combo {
		rtf = netpollDeadline
	}

	// 如果没有设置读 deadline 时间点运行的函数，则使用默认的函数
	if pd.rt.f == nil {
		if pd.rd > 0 {
			pd.rt.f = rtf
			// Copy current seq into the timer arg.
			// Timer func will check the seq against current descriptor seq,
			// if they differ the descriptor was reused or timers were reset.
			pd.rt.arg = pd.makeArg()
			pd.rt.seq = pd.rseq
			resettimer(&pd.rt, pd.rd)
		}
	} else if pd.rd != rd0 || combo != combo0 {
		pd.rseq++ // invalidate current timers
		if pd.rd > 0 {
			modtimer(&pd.rt, pd.rd, 0, rtf, pd.makeArg(), pd.rseq)
		} else {
			deltimer(&pd.rt)
			pd.rt.f = nil
		}
	}

	// 此处处理写 deadline 相关的情况，与读类似
	if pd.wt.f == nil {
		if pd.wd > 0 && !combo {
			pd.wt.f = netpollWriteDeadline
			pd.wt.arg = pd.makeArg()
			pd.wt.seq = pd.wseq
			resettimer(&pd.wt, pd.wd)
		}
	} else if pd.wd != wd0 || combo != combo0 {
		pd.wseq++ // invalidate current timers
		if pd.wd > 0 && !combo {
			modtimer(&pd.wt, pd.wd, 0, netpollWriteDeadline, pd.makeArg(), pd.wseq)
		} else {
			deltimer(&pd.wt)
			pd.wt.f = nil
		}
	}

	// If we set the new deadline in the past, unblock currently pending IO if any.
	var rg, wg *g
	// 如果 deadline 时间点早于当前时间，则 unblock pd 的所有 IO，返回 timeout IO 错误
	if pd.rd < 0 || pd.wd < 0 {
		atomic.StorepNoWB(noescape(unsafe.Pointer(&wg)), nil) // full memory barrier between stores to rd/wd and load of rg/wg in netpollunblock
		if pd.rd < 0 {
			rg = netpollunblock(pd, 'r', false)
		}
		if pd.wd < 0 {
			wg = netpollunblock(pd, 'w', false)
		}
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

// 获取 pd 读/写阻塞的 goroutine 并将其状态切换为 runnable，poll_runtime_pollUnblock 函数一般在 `关闭连接` 时使用。
//go:linkname poll_runtime_pollUnblock internal/poll.runtime_pollUnblock
func poll_runtime_pollUnblock(pd *pollDesc) {
	lock(&pd.lock)
	if pd.closing {
		throw("runtime: unblock on closing polldesc")
	}

	// 将 pd 状态置为 closing, poll_runtime_pollClose 会停止并回收 pd
	pd.closing = true
	pd.rseq++
	pd.wseq++
	var rg, wg *g
	atomic.StorepNoWB(noescape(unsafe.Pointer(&rg)), nil) // full memory barrier between store to closing and read of rg/wg in netpollunblock

	// 获取读写对应的 goroutine。因此此处 ioready 设置为false，表示非底层 IO 的操作，底层 epoll 上报事件后，会通过 runtime.netpollready 调用
	// netpollunblock 函数，此时 netpollunblock 的第三个参数会变为 true，用于将对应事件的 goroutine 变为非阻塞，处理 epoll 的读写事件
	rg = netpollunblock(pd, 'r', false)
	wg = netpollunblock(pd, 'w', false)

	// pd.rt.f 和 pd.wt.f 都是定时器超时后执行的函数，如果这些函数非空，则清除定时器并置为初始值 nil(后续 pd 需要回收)。
	// `定时器` 用于设置读写连接的 deadline 时间点
	if pd.rt.f != nil {
		deltimer(&pd.rt)
		pd.rt.f = nil
	}
	if pd.wt.f != nil {
		deltimer(&pd.wt)
		pd.wt.f = nil
	}
	unlock(&pd.lock)

	// 此处才是真正 unblock 阻塞的 goroutine，netpollgoready 会调用 goready 函数将阻塞的 goroutine 变为 runnable 状态，继续执行。
	// 此处的 unblock 操作对应调用 netpollblock 函数 gopark 的 goroutine，如等待 Accept，等待 Read 等。当 Accept 有连接到达或 Read 有数据读取
	// 时，这时需要 unblock 对应的 goroutine 继续处理(当然也包括处理错误场景)
	if rg != nil {
		netpollgoready(rg, 3)
	}
	if wg != nil {
		netpollgoready(wg, 3)
	}
}

// netpollready is called by the platform-specific netpoll function.
// It declares that the fd associated with pd is ready for I/O.
// The toRun argument is used to build a list of goroutines to return
// from netpoll. The mode argument is 'r', 'w', or 'r'+'w' to indicate
// whether the fd is ready for reading or writing or both.
//
// This may run while the world is stopped, so write barriers are not allowed.

// netpollready 调用 netpollunblock 返回就绪 fd 对应的 goroutine 的抽象数据结构 g

//go:nowritebarrier
func netpollready(toRun *gList, pd *pollDesc, mode int32) {
	var rg, wg *g
	if mode == 'r' || mode == 'r'+'w' {
		rg = netpollunblock(pd, 'r', true)
	}
	if mode == 'w' || mode == 'r'+'w' {
		wg = netpollunblock(pd, 'w', true)
	}
	if rg != nil {
		toRun.push(rg)
	}
	if wg != nil {
		toRun.push(wg)
	}
}

func netpollcheckerr(pd *pollDesc, mode int32) int {
	if pd.closing {
		return pollErrClosing
	}
	if (mode == 'r' && pd.rd < 0) || (mode == 'w' && pd.wd < 0) {
		return pollErrTimeout
	}
	// Report an event scanning error only on a read event.
	// An error on a write event will be captured in a subsequent
	// write call that is able to report a more specific error.
	if mode == 'r' && pd.everr {
		return pollErrNotPollable
	}
	return pollNoError
}

// netpollblockcommit 在 gopark 函数里被调用
func netpollblockcommit(gp *g, gpp unsafe.Pointer) bool {
	// 通过原子操作把当前 goroutine 抽象的数据结构 g，也就是这里的参数 gp 存入 gpp 指针，
	// 此时 gpp 的值是 pollDesc 的 rg 或者 wg 指针
	r := atomic.Casuintptr((*uintptr)(gpp), pdWait, uintptr(unsafe.Pointer(gp)))
	if r {
		// Bump the count of goroutines waiting for the poller.
		// The scheduler uses this to decide whether to block
		// waiting for the poller if there is nothing else to do.
		atomic.Xadd(&netpollWaiters, 1)
	}
	return r
}

func netpollgoready(gp *g, traceskip int) {
	atomic.Xadd(&netpollWaiters, -1)
	// 这里调用了 goready
	goready(gp, traceskip+1)
}

// returns true if IO is ready, or false if timedout or closed
// waitio - wait only for completed IO, ignore errors
// Concurrent calls to netpollblock in the same mode are forbidden, as pollDesc
// can hold only a single waiting goroutine for each mode.
// runtime.netpollblock 是 Goroutine 等待 I/O 事件的关键函数，它会使用运行时提供的 runtime.gopark 让出当前线程，将 Goroutine 转换到休眠状态并等待运行时的唤醒。
func netpollblock(pd *pollDesc, mode int32, waitio bool) bool {
	// gpp 保存的是 goroutine 的数据结构 g，这里会根据 mode 的值决定是 rg 还是 wg，
	// 前面提到过，rg 和 wg 是用来保存等待 I/O 就绪的 gorouine 的，后面调用 gopark 之后，
	// 会把当前的 goroutine 的抽象数据结构 g 存入 gpp 这个指针，也就是 rg 或者 wg
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// set the gpp semaphore to pdWait
	// 这个 for 循环是为了等待 io ready 或者 io wait
	for {
		// Consume notification if already ready.
		if atomic.Casuintptr(gpp, pdReady, 0) {
			return true
		}
		// 如果没有期待的 I/O 事件发生，则通过原子操作把 gpp 的值置为 pdWait 并退出 for 循环
		if atomic.Casuintptr(gpp, 0, pdWait) {
			break
		}

		// Double check that this isn't corrupt; otherwise we'd loop
		// forever.
		if v := atomic.Loaduintptr(gpp); v != pdReady && v != 0 {
			throw("runtime: double wait")
		}
	}

	// need to recheck error states after setting gpp to pdWait
	// this is necessary because runtime_pollUnblock/runtime_pollSetDeadline/deadlineimpl
	// do the opposite: store to closing/rd/wd, membarrier, load of rg/wg
	// waitio 此时是 false，netpollcheckerr 方法会检查当前 pollDesc 对应的 fd 是否是正常的，

	// 通常来说  netpollcheckerr(pd, mode) == 0 是成立的，所以这里会执行 gopark
	// 把当前 goroutine 给 park 住，直至对应的 fd 上发生可读/可写或者其他『期待的』I/O 事件为止，
	// 然后 unpark 返回，在 gopark 内部会把当前 goroutine 的抽象数据结构 g 存入
	// gpp(pollDesc.rg/pollDesc.wg) 指针里，以便在后面的 netpoll 函数取出 pollDesc 之后，
	// 把 g 添加到链表里返回，接着重新调度 goroutine
	if waitio || netpollcheckerr(pd, mode) == pollNoError {
		// 注册 netpollblockcommit 回调给 gopark，在 gopark 内部会执行它，保存当前 goroutine 到 gpp
		gopark(netpollblockcommit, unsafe.Pointer(gpp), waitReasonIOWait, traceEvGoBlockNet, 5)
	}
	// be careful to not lose concurrent pdReady notification
	old := atomic.Xchguintptr(gpp, 0)
	if old > pdWait {
		throw("runtime: corrupted polldesc")
	}
	return old == pdReady
}

// 这个函数的名字有点奇怪，它并不能主动 unblock goroutine
//
// netpollunblock 会依据传入的 mode 决定从 pollDesc 的 rg 或者 wg， 取出当时 gopark 之时存入的 goroutine
func netpollunblock(pd *pollDesc, mode int32, ioready bool) *g {
	// mode == 'r' 代表当时 gopark 是为了等待读事件，而 mode == 'w' 则代表是等待写事件
	gpp := &pd.rg
	if mode == 'w' {
		gpp = &pd.wg
	}

	// 使用 for 循环用于执行原子操作。一个 pd 对于一条连接，而一条连接可能被多个 goroutine 操作。
	for {
		// 取出 gpp 存储的 g
		old := atomic.Loaduintptr(gpp)

		// 如果 gpp 为 pdReady，则对应的 goroutine 为 unblock 状态，返回即可
		if old == pdReady {
			return nil
		}

		// 此处用于初始状态时的场景，此时并没有调用 netpollblock 阻塞 goroutine，直接返回即可
		if old == 0 && !ioready {
			// Only set pdReady for ioready. runtime_pollWait
			// will check for timeout/cancel before waiting.
			return nil
		}
		var new uintptr
		if ioready {
			new = pdReady
		}

		// 重置 pollDesc 的 rg 或者 wg
		// atomic.Casuintptr 的实现在runtime/internal/atomic/asm_${plantform}.s中，如下处理逻辑为
		//  if(*val == *old){
		//      *val = new;
		//      return 1;
		//  } else {
		//      return 0;
		//  }
		// 该函数的实现了 `自旋锁`, for 循环执行该原子操作。这里的原子循环应该是多 goroutine 操作同一个 pd 场景下等待一个相对稳定的状态，因为按照本
		// 函数代码逻辑来看，*gpp 一定等于 *old
		if atomic.Casuintptr(gpp, old, new) {
			// 如果该 goroutine 还是必须等待，则返回 nil
			if old == pdWait {
				old = 0
			}
			// 通过万能指针还原成 g 并返回
			// 返回阻塞在 pd.rg/pd.wg 上的 goroutine 地址
			return (*g)(unsafe.Pointer(old))
		}
	}
}

func netpolldeadlineimpl(pd *pollDesc, seq uintptr, read, write bool) {
	lock(&pd.lock)
	// Seq arg is seq when the timer was set.
	// If it's stale, ignore the timer event.
	currentSeq := pd.rseq
	if !read {
		currentSeq = pd.wseq
	}
	if seq != currentSeq {
		// The descriptor was reused or timers were reset.
		unlock(&pd.lock)
		return
	}
	var rg *g
	if read {
		if pd.rd <= 0 || pd.rt.f == nil {
			throw("runtime: inconsistent read deadline")
		}
		pd.rd = -1
		atomic.StorepNoWB(unsafe.Pointer(&pd.rt.f), nil) // full memory barrier between store to rd and load of rg in netpollunblock
		rg = netpollunblock(pd, 'r', false)
	}
	var wg *g
	if write {
		if pd.wd <= 0 || pd.wt.f == nil && !read {
			throw("runtime: inconsistent write deadline")
		}
		pd.wd = -1
		atomic.StorepNoWB(unsafe.Pointer(&pd.wt.f), nil) // full memory barrier between store to wd and load of wg in netpollunblock
		wg = netpollunblock(pd, 'w', false)
	}
	unlock(&pd.lock)
	if rg != nil {
		netpollgoready(rg, 0)
	}
	if wg != nil {
		netpollgoready(wg, 0)
	}
}

func netpollDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, true)
}

func netpollReadDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, true, false)
}

func netpollWriteDeadline(arg any, seq uintptr) {
	netpolldeadlineimpl(arg.(*pollDesc), seq, false, true)
}

func (c *pollCache) alloc() *pollDesc {
	lock(&c.lock)
	if c.first == nil {
		const pdSize = unsafe.Sizeof(pollDesc{})
		n := pollBlockSize / pdSize
		if n == 0 {
			n = 1
		}
		// Must be in non-GC memory because can be referenced
		// only from epoll/kqueue internals.
		mem := persistentalloc(n*pdSize, 0, &memstats.other_sys)
		for i := uintptr(0); i < n; i++ {
			pd := (*pollDesc)(add(mem, i*pdSize))
			pd.link = c.first
			c.first = pd
		}
	}
	pd := c.first
	c.first = pd.link
	lockInit(&pd.lock, lockRankPollDesc)
	unlock(&c.lock)
	return pd
}

// makeArg converts pd to an interface{}.
// makeArg does not do any allocation. Normally, such
// a conversion requires an allocation because pointers to
// go:notinheap types (which pollDesc is) must be stored
// in interfaces indirectly. See issue 42076.
func (pd *pollDesc) makeArg() (i any) {
	x := (*eface)(unsafe.Pointer(&i))
	x._type = pdType
	x.data = unsafe.Pointer(&pd.self)
	return
}

var (
	pdEface any    = (*pollDesc)(nil)
	pdType  *_type = efaceOf(&pdEface)._type
)
