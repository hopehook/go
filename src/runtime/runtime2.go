// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/goarch"
	"runtime/internal/atomic"
	"unsafe"
)

// defined constants
const (
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack (and hence its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	//
	// TODO(austin): The _Gscan bit could be much lighter-weight.
	// For example, we could choose not to run _Gscanrunnable
	// goroutines found in the run queue, rather than CAS-looping
	// until they become _Grunnable. And transitions like
	// _Gscanwaiting -> _Gscanrunnable are actually okay because
	// they don't affect stack ownership.

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	_Grunnable // 1

	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (g.m and g.m.p are valid).
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.g., a channel wait
	// queue) so it can be ready()d when necessary. The stack is
	// not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a stack
	// allocated. The G and its stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	_Gcopystack // 8

	// _Gpreempted means this goroutine stopped itself for a
	// suspendG preemption. It is like _Gwaiting, but nothing is
	// yet responsible for ready()ing it. Some suspendG must CAS
	// the status to _Gwaiting to take responsibility for
	// ready()ing this G.
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. Typically, it's on the idle P list and available
	// to the scheduler, but it may just be transitioning between
	// other states.
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty.
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.g., to schedule a locked G).
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity to an M in a syscall but is not owned by it and
	// may be stolen by another M. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P. Note that there's an ABA hazard: even if
	// an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim.
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.g., trace buffers).
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
type mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	lockRankStruct
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.g. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user g.
//
// note 的底层实现机制跟操作系统相关，不同系统使用不同的机制
// 比如 linux 下使用的 futex 系统调用
// 而 mac 下则是使用的 pthread_cond_t 条件变量
// note 对这些底层机制做了一个抽象和封装。
//
// 这种封装给扩展性带来了很大的好处，比如当睡眠和唤醒功能需要支持新平台时，
// 只需要在 note 层增加对特定平台的支持即可，不需要修改上层的任何代码。
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// 函数，在 GO 语言中属于头等对象，可以被当作参数传递、也可以作为函数返回值、绑定到变量。
// Go语言称这样的参数、返回值和变量为 “Function Value”。
//
// Function Value 本质上是一个指针，却不直接指向函数指令入口，而是指向 runtime.funcval 结构体。
//  （1）Go 语言里 Function Value 本质上是指向 funcval 结构体的指针；
//  （2）Go 语言里 "闭包" 只是拥有捕获列表的 Function Value；
//  （3）捕获变量在外层函数与闭包函数中要保持一致。
type funcval struct {
	// 这个地址才是函数的指令入口
	fn uintptr
	// variable-size, fn-specific data here
}

// 非空接口，只可以容纳实现了该接口的元素
//
// 接口要求实现的方法列表与动态类型的方法集都是有序的，所以经过一次循环对比就可以确定该动态类型是否实现了特定接口：
//  （1）如果没有实现，那么 itab.fun[0] 就等于0；
//  （2）如果实现了，就把动态类型实现的方法地址存储到 itab.fun 数组中。
// 有了这样的 itab，再通过接口调用方法时，就可以像 C++ 的虚函数那样直接按数组下标读取地址了。
type iface struct {
	// 表示接口的类型，以及赋给这个接口的实体类型。
	tab *itab

	// 万能指针，一般而言是一个指向 `堆内存` 的指针。
	data unsafe.Pointer
}

// 空接口，可以容纳任何元素
type eface struct {
	// 容纳的元素 类型元数据
	_type *_type

	// 容纳的元素
	// 如果元素是个指针，直接存储指针即可；
	// 如果元素是个值，需要隐士分配一个内部变量，然后将它的地址赋值给 data 字段
	data unsafe.Pointer
}

func efaceOf(ep *any) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
// 1. Never hold an muintptr locally across a safe point.
//
// 2. Any muintptr in the heap must be owned by the M itself so it can
//    ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

// gobuf 存储 goroutine 调度 `上下文信息` 的结构体
type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	sp   uintptr  // 存储 CPU 的 rsp 寄存器的值
	pc   uintptr  // 存储 CPU 的 rip 寄存器的值
	g    guintptr // 持有当前 gobuf 的 goroutine 的 “指针”
	ctxt unsafe.Pointer

	// 保存系统调用的返回值，因为从系统调用返回之后如果 p 被其它工作线程抢占，
	// 则这个 goroutine 会被放入全局运行队列被其它工作线程调度，
	// 其它线程需要知道系统调用的返回值。
	ret uintptr // 保存系统调用的返回值

	lr uintptr

	// 保存 CPU 的 rbp 寄存器的值
	bp uintptr // for framepointer-enabled architectures
}

// sudog represents a g in a wait list, such as for sending/receiving
// on a channel.
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.

// channel 用封装的 sudog 代替接收双方的 g，因为要携带数据项，存储相关状态
// sudog 实现了二级缓存复用链表 P.sudogcache、schedt.sudogcache
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *g

	next *sudog
	prev *sudog
	elem unsafe.Pointer // data element (may point to stack)

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64
	releasetime int64
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	isSelect bool

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	success bool

	parent   *sudog // semaRoot binary tree
	waitlink *sudog // g.waiting list or semaRoot
	waittail *sudog // semaRoot
	c        *hchan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr
	err  uintptr // error number
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
// 描述栈的数据结构，栈的范围：[lo, hi)
//
// stack 描述了 Goroutine 的执行栈，栈的区间为 [lo, hi)，在栈两边没有任何隐式数据结构
// 因此 Go 的执行栈由运行时管理，本质上分配在堆中，比 ulimit -s 大
type stack struct {
	lo uintptr // 栈顶，低地址
	hi uintptr // 栈底，高地址
}

// heldLockInfo gives info on a held lock and the rank of that lock
type heldLockInfo struct {
	lockAddr uintptr
	rank     lockRank
}

// 它代表了一个 goroutine
type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	// 执行栈: stack.lo 和 stack.hi 成员描述了栈的下界和上界内存地址
	stack stack // offset known to runtime/cgo

	// 用于栈的扩张和收缩检查，抢占标志
	//
	// stackguard0 是对比 Go 栈增长的 prologue 的栈指针
	// 如果 sp 寄存器比 stackguard0 小（由于栈往低地址方向增长），会触发栈拷贝和调度
	// 通常情况下：stackguard0 = stack.lo + StackGuard，但被抢占时会变为 StackPreempt
	stackguard0 uintptr // offset known to liblink

	// stackguard1 是对比 C 栈增长的 prologue 的栈指针
	// 当位于 g0 和 gsignal 栈上时，值为 stack.lo + StackGuard
	// 在其他栈上值为 ~0 用于触发 morestackc (并 crash) 调用
	stackguard1 uintptr // offset known to liblink

	_panic *_panic // innermost panic - offset known to liblink
	_defer *_defer // innermost defer

	// 当前绑定的 m
	m *m // current m; offset known to arm liblink

	// 切换时，用于保存 g 的上下文
	sched     gobuf   // goroutine
	syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc to use during gc
	stktopsp  uintptr // expected sp at top of stack, to check in traceback

	// param is a generic pointer parameter field used to pass
	// values in particular contexts where other storage for the
	// parameter would be difficult to find. It is currently used
	// in three ways:
	// 1. When a channel operation wakes up a blocked goroutine, it sets param to
	//    point to the sudog of the completed blocking operation.
	// 2. By gcAssistAlloc1 to signal back to its caller that the goroutine completed
	//    the GC cycle. It is unsafe to do so in any other way, because the goroutine's
	//    stack may have moved in the meantime.
	// 3. By debugCallWrap to pass parameters to a new goroutine because allocating a
	//    closure in the runtime is forbidden.
	// 用于传递参数，睡眠时其他 goroutine 可以设置 param，唤醒时该 goroutine 可以获取
	// wakeup 时传入的参数
	param        unsafe.Pointer
	atomicstatus uint32
	stackLock    uint32 // sigprof/scang lock; TODO: fold in to atomicstatus
	// 唯一的 goroutine 的ID
	goid int64
	// schedlink 字段指向全局运行队列中的下一个g，
	// 所有位于全局运行队列中的 g 形成一个链表
	schedlink guintptr
	// g 被阻塞的大体时间
	waitsince int64 // approx time when the g become blocked
	// g 被阻塞的原因
	waitreason waitReason // if status==Gwaiting

	// 标记是否可抢占
	// 抢占调度标志。这个为 true 时，stackguard0 等于 stackpreempt
	preempt       bool // preemption signal, duplicates stackguard0 = stackpreempt
	preemptStop   bool // transition to _Gpreempted on preemption; otherwise, just deschedule
	preemptShrink bool // shrink stack at synchronous safe point

	// asyncSafePoint is set if g is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	asyncSafePoint bool

	paniconfault bool // panic (instead of crash) on unexpected fault address
	gcscandone   bool // g has scanned stack; protected by _Gscan bit in status
	throwsplit   bool // must not split stack
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	activeStackChans bool
	// parkingOnChan indicates that the goroutine is about to
	// park on a chansend or chanrecv. Used to signal an unsafe point
	// for stack shrinking. It's a boolean value, but is updated atomically.
	parkingOnChan uint8

	raceignore     int8  // ignore race detection events
	sysblocktraced bool  // StartTrace has emitted EvGoInSyscall about this goroutine
	tracking       bool  // whether we're tracking this G for sched latency statistics
	trackingSeq    uint8 // used to decide whether to track this G
	runnableStamp  int64 // timestamp of when the G last became runnable, only used when tracking
	runnableTime   int64 // the amount of time spent runnable, cleared when running, only used when tracking
	// syscall 返回之后的 cputicks，用来做 tracing
	sysexitticks int64    // cputicks when syscall has returned (for tracing)
	traceseq     uint64   // trace event sequencer
	tracelastp   puintptr // last P emitted an event for this goroutine
	// G 被锁定只在这个 m 上运行
	// 如果调用了 LockOsThread，那么这个 g 会绑定到某个 m 上
	lockedm  muintptr
	sig      uint32
	writebuf []byte
	sigcode0 uintptr
	sigcode1 uintptr
	sigpc    uintptr
	// 调用者的 PC/IP
	// 创建该 goroutine 的语句的指令地址
	gopc      uintptr         // pc of go statement that created this goroutine // 调用者 PC/IP
	ancestors *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	// 任务函数
	// goroutine 函数的指令地址
	startpc uintptr // pc of goroutine function // 任务函数
	racectx uintptr
	waiting *sudog         // sudog structures this g is waiting on (that have a valid elem ptr); in lock order
	cgoCtxt []uintptr      // cgo traceback context
	labels  unsafe.Pointer // profiler labels

	// time.Sleep 缓存的定时器
	timer *timer // cached timer for time.Sleep

	// 避免 select watch 的多个 channel 同时就绪，导致竞争。
	selectDone uint32 // are we participating in a select and did someone win the race?

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	gcAssistBytes int64
}

// gTrackingPeriod is the number of transitions out of _Grunning between
// latency tracking runs.
const gTrackingPeriod = 8

const (
	// tlsSlots is the number of pointer-sized slots reserved for TLS on some platforms,
	// like Windows.
	tlsSlots = 6
	tlsSize  = tlsSlots * goarch.PtrSize
)

// m 代表工作线程，保存了自身使用的栈信息
type m struct {
	// 用来执行调度的 goroutine, g0
	// 提供系统栈空间, g0 栈

	// 记录 `内核线程` 使用的栈信息。在执行调度代码时需要使用
	// 执行用户 goroutine 代码时，使用用户 goroutine 自己的栈，因此调度时会发生栈的切换
	g0      *g     // goroutine with scheduling stack
	morebuf gobuf  // gobuf arg to morestack
	divmod  uint32 // div/mod denominator for arm - known to liblink

	// Fields not known to debuggers.
	procid uint64 // for debuggers, but offset not hard-coded
	// 处理信号的 goroutine
	gsignal    *g           // signal-handling g
	goSigStack gsignalStack // Go-allocated signal handling stack
	sigmask    sigset       // storage for saved signal mask

	// 通过 tls 结构体实现 m 与工作线程的绑定
	// 这里是线程本地存储
	tls      [tlsSlots]uintptr // thread-local storage (for x86 extern register)
	mstartfn func()            // 启动函数

	// 当前运行的 goroutine
	curg *g // current running goroutine // 当前运行的 G

	caughtsig guintptr // goroutine running during fatal signal

	// 绑定的 P
	p puintptr // attached p for executing go code (nil if not executing go code)
	// 临时存放 P
	nextp puintptr
	oldp  puintptr // the p that was attached before executing a syscall

	id int64

	mallocing int32
	throwing  int32
	// 该字段不等于空字符串的话，要保持 curg 始终在这个 m 上运行
	preemptoff string // if != "", keep curg running on this m
	// locks 表示该 M 是否被锁的状态，M 被锁的状态下该 M 无法执行 GC
	locks     int32
	dying     int32
	profilehz int32
	// 是否自旋，自旋就表示 M 正在找 G 来运行
	spinning bool // m is out of work and is actively looking for work
	// m 是否被阻塞
	blocked     bool // m is blocked on a note
	newSigstack bool // minit on C thread called sigaltstack
	printlock   int8

	// 正在执行 cgo 调用
	incgo      bool   // m is executing a cgo call
	freeWait   uint32 // if == 0, safe to free g0 and delete m (atomic)
	fastrand   uint64
	needextram bool
	traceback  uint8

	// cgo 调用总计数
	ncgocall      uint64      // number of cgo calls in total
	ncgo          int32       // number of cgo calls currently in progress
	cgoCallersUse uint32      // if non-zero, cgoCallers in use temporarily
	cgoCallers    *cgoCallers // cgo traceback if crashing in cgo call
	doesPark      bool        // non-P running threads: sysmon and newmHandoff never use .park

	// 没有 goroutine 需要运行时，工作线程睡眠在这个 park 成员上，
	// 其它线程通过这个 park 唤醒该工作线程
	park note // 休眠锁

	// 用于链接 allm，记录所有工作线程的链表
	alllink   *m // on allm
	schedlink muintptr
	// 锁定 g 在当前 m 上执行，而不会切换到其他 m，一般 cgo 调用或者手动调用 LockOSThread() 才会有值
	lockedg guintptr
	// thread 创建的栈
	createstack [32]uintptr // stack that created this thread.
	// 用户锁定 M 的标记
	lockedExt uint32 // tracking for external LockOSThread
	// runtime 内部锁定 M 的标记
	lockedInt uint32 // tracking for internal lockOSThread
	// 正在等待锁的下一个 m
	nextwaitm     muintptr // next m waiting for lock
	waitunlockf   func(*g, unsafe.Pointer) bool
	waitlock      unsafe.Pointer
	waittraceev   byte
	waittraceskip int
	startingtrace bool
	syscalltick   uint32
	freelink      *m // on sched.freem

	// mFixup is used to synchronize OS related m state
	// (credentials etc) use mutex to access. To avoid deadlocks
	// an atomic.Load() of used being zero in mDoFixupFn()
	// guarantees fn is nil.
	mFixup struct {
		lock mutex
		used uint32
		fn   func(bool) bool
	}

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	libcall   libcall
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // stores syscall parameters on windows

	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails. Accessed atomically.
	preemptGen uint32

	// Whether this is a pending preemption signal on this M.
	// Accessed atomically.
	signalPending uint32

	dlogPerM

	mOS

	// Up to 10 locks held by this m, maintained by the lock ranking code.
	locksHeldLen int
	locksHeld    [10]heldLockInfo
}

// p 保存 go 运行时所必须的资源
type p struct {
	// id 也是 allp 的数组下标
	id     int32
	status uint32 // one of pidle/prunning/...
	// 单向链表，指向下一个 P 的地址
	link puintptr
	// 每次调用 schedule 时会加一
	schedtick uint32 // incremented on every scheduler call
	// 每一次系统调用加 1
	syscalltick uint32 // incremented on every system call
	// 用于 sysmon 线程记录被监控 p 的系统调用时间和运行时间
	sysmontick sysmontick // last tick observed by sysmon
	// 指向绑定的 m，如果 p 是 idle 的话，那这个指针是 nil
	m           muintptr // back-link to associated m (nil if idle)
	mcache      *mcache
	pcache      pageCache
	raceprocctx uintptr

	deferpool    []*_defer // pool of available defer structs (see panic.go)
	deferpoolbuf [32]*_defer

	// Cache of goroutine ids, amortizes accesses to runtime·sched.goidgen.
	// goroutine 的 ID 的缓存
	goidcache    uint64
	goidcacheend uint64

	// Queue of runnable goroutines. Accessed without lock.
	// 本地可运行的 goroutine 队列，无锁
	runqhead uint32
	runqtail uint32
	// 使用数组实现的循环队列，最多 256 个 goroutine
	runq [256]guintptr

	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	//
	// Note that while other P's may atomically CAS this to zero,
	// only the owner P can CAS it to a valid G.
	// 下一个运行的 g，优先级最高
	runnext guintptr

	// Available G's (status == Gdead)
	// P 本地可复用的 G 对象 （dead 状态的 G）
	gFree struct {
		gList
		n int32
	}

	sudogcache []*sudog
	sudogbuf   [128]*sudog

	// Cache of mspan objects from the heap.
	mspancache struct {
		// We need an explicit length here because this field is used
		// in allocation codepaths where write barriers are not allowed,
		// and eliminating the write barrier/keeping it eliminated from
		// slice updates is tricky, moreso than just managing the length
		// ourselves.
		len int
		buf [128]*mspan
	}

	tracebuf traceBufPtr

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	traceSweep bool
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	traceSwept, traceReclaimed uintptr

	palloc persistentAlloc // per-P to avoid mutex

	_ uint32 // Alignment for atomic fields below

	// The when field of the first entry on the timer heap.
	// This is updated using atomic functions.
	// This is 0 if the timer heap is empty.
	timer0When uint64

	// The earliest known nextwhen field of a timer with
	// timerModifiedEarlier status. Because the timer may have been
	// modified again, there need not be any timer with this value.
	// This is updated using atomic functions.
	// This is 0 if there are no timerModifiedEarlier timers.
	timerModifiedEarliest uint64

	// Per-P GC state
	gcAssistTime         int64 // Nanoseconds in assistAlloc
	gcFractionalMarkTime int64 // Nanoseconds in fractional mark worker (atomic)

	// gcMarkWorkerMode is the mode for the next mark worker to run in.
	// That is, this is used to communicate with the worker goroutine
	// selected for immediate execution by
	// gcController.findRunnableGCWorker. When scheduling other goroutines,
	// this field must be set to gcMarkWorkerNotWorker.
	gcMarkWorkerMode gcMarkWorkerMode
	// gcMarkWorkerStartTime is the nanotime() at which the most recent
	// mark worker started.
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	gcw gcWork

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	wbBuf wbBuf

	runSafePointFn uint32 // if 1, run sched.safePointFn at next safe point

	// statsSeq is a counter indicating whether this P is currently
	// writing any stats. Its value is even when not, odd when it is.
	statsSeq uint32

	// Lock for timers. We normally access the timers while running
	// on this P, but the scheduler can also do it from a different P.
	timersLock mutex

	// Actions to take at some time. This is used to implement the
	// standard library's time package.
	// Must hold timersLock to access.
	timers []*timer

	// Number of timers in P's heap.
	// Modified using atomic instructions.
	numTimers uint32

	// Number of timerDeleted timers in P's heap.
	// Modified using atomic instructions.
	deletedTimers uint32

	// Race context used while executing timer functions.
	timerRaceCtx uintptr

	// scannableStackSizeDelta accumulates the amount of stack space held by
	// live goroutines (i.e. those eligible for stack scanning).
	// Flushed to gcController.scannableStackSize once scannableStackSizeSlack
	// or -scannableStackSizeSlack is reached.
	scannableStackSizeDelta int64

	// preempt is set to indicate that this P should be enter the
	// scheduler ASAP (regardless of what G is running on it).
	// 抢占调度标志，如果需要抢占调度，设置 preempt 为true
	preempt bool

	// Padding is no longer needed. False sharing is now not a worry because p is large enough
	// that its size class is an integer multiple of the cache line size (for any of our architectures).
}

// 保存调度的全局信息
type schedt struct {
	// accessed atomically. keep at top to ensure alignment on 32-bit systems.
	// 需以原子访问访问。
	// 保持在 struct 顶部，以使其在 32 位系统上可以对齐
	goidgen   uint64
	lastpoll  uint64 // time of last network poll, 0 if currently polling
	pollUntil uint64 // time to which current poll is sleeping

	lock mutex

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().

	// 由空闲的工作线程 M 组成的链表，这些 M 没有 G 可以执行，等待唤醒
	midle muintptr // idle m's waiting for work // 闲置的 M 链表
	// 空闲的工作线程数量
	nmidle int32 // number of idle m's waiting for work
	// 空闲的且被 lock 的 m 计数
	nmidlelocked int32 // number of locked m's waiting for work
	mnext        int64 // number of m's that have been created and next M ID
	// 表示最多所能创建的工作线程数量
	maxmcount int32 // maximum number of m's allowed (or die)
	nmsys     int32 // number of system m's not counted for deadlock
	nmfreed   int64 // cumulative number of freed m's

	// goroutine 的数量，自动更新
	ngsys uint32 // number of system goroutines; updated atomically

	// 由空闲的 p 结构体对象组成的链表
	pidle puintptr // idle p's
	// 空闲的 p 结构体对象的数量
	npidle     uint32
	nmspinning uint32 // See "Worker thread parking/unparking" comment in proc.go.

	// Global runnable queue.
	// 全局可运行的 G队列
	runq gQueue
	// 元素数量
	runqsize int32

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	disable struct {
		// user disables scheduling of user goroutines.
		user     bool
		runnable gQueue // pending runnable Gs
		n        int32  // length of runnable
	}

	// Global cache of dead G's.
	// schedt 下保存的全局可复用 G 对象链表 （如果 P 本地拿不到，就会从这里拿）
	gFree struct {
		lock    mutex
		stack   gList // Gs with stacks
		noStack gList // Gs without stacks
		n       int32 // 空闲 g 的数量
	}

	// Central cache of sudog structs.
	sudoglock  mutex
	sudogcache *sudog

	// Central pool of available defer structs.
	// 不同大小的可用的 defer struct 的集中缓存池
	deferlock mutex
	deferpool *_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	freem *m

	gcwaiting  uint32 // gc is waiting to run
	stopwait   int32
	stopnote   note
	sysmonwait uint32
	sysmonnote note

	// While true, sysmon not ready for mFixup calls.
	// Accessed atomically.
	sysmonStarting uint32

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	safePointFn   func(*p)
	safePointWait int32
	safePointNote note

	profilehz int32 // cpu profiling rate

	// 上次修改 gomaxprocs 的纳秒时间
	procresizetime int64 // nanotime() of last change to gomaxprocs
	totaltime      int64 // ∫gomaxprocs dt up to procresizetime

	// sysmonlock protects sysmon's actions on the runtime.
	//
	// Acquire and hold this mutex to block sysmon from interacting
	// with the rest of the runtime.
	sysmonlock mutex

	_ uint32 // ensure timeToRun has 8-byte alignment

	// timeToRun is a distribution of scheduling latencies, defined
	// as the sum of time a G spends in the _Grunnable state before
	// it transitions to _Grunning.
	//
	// timeToRun is protected by sched.lock.
	timeToRun timeHistogram
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // add SA_ONSTACK to libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
type _func struct {
	entryoff uint32 // start pc, as offset from moduledata.text/pcHeader.textStart
	nameoff  int32  // function name

	args        int32  // in/out args size
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

	pcsp      uint32
	pcfile    uint32
	pcln      uint32
	npcdata   uint32
	cuOffset  uint32 // runtime.cutab offset of this function's CU
	funcID    funcID // set for certain special runtime functions
	flag      funcFlag
	_         [1]byte // pad
	nfuncdata uint8   // must be last, must end on a uint32-aligned boundary
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
type funcinl struct {
	ones  uint32  // set to ^0 to distinguish from _func
	entry uintptr // entry of the real (the "outermost") frame
	name  string
	file  string
	line  int
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory
// Needs to be in sync with
// ../cmd/compile/internal/reflectdata/reflect.go:/^func.WriteTabs.
type itab struct {

	// 接口的类型元数据
	// 记录着 「该接口要求实现的方法列表」
	inter *interfacetype

	// 实体的类型元数据，从 _type 到 uncommontype，再到 [mcount]method，可以找到 「该动态类型的方法集」。
	_type *_type

	hash uint32 // copy of _type.hash. Used for type switches.
	_    [4]byte

	// fun 字段放置和接口方法对应的具体数据类型的方法地址，实现接口调用方法的动态分派，
	// 一般在每次给接口赋值发生转换时会更新此表，或者直接拿缓存的 itab。
	//
	// 为什么 fun 数组的大小为 1，要是接口定义了多个方法可怎么办？
	// 实际上，这里存储的是第一个方法的函数指针，如果有更多的方法，在它之后的内存空间里继续存储。
	// 从汇编角度来看，通过增加地址就能获取到这些函数指针，没什么影响。
	// 顺便提一句，这些方法是按照函数名称的字典序进行排列的。
	fun [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

// Lock-free stack node.
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle uint32
}

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < goarch.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in freedefer and deferProcStack
// This struct must match the code in cmd/compile/internal/ssagen/ssa.go:deferstruct
// and cmd/compile/internal/ssagen/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
type _defer struct {
	started bool
	heap    bool
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	// 表示当前 defer 是否经过开放编码的优化
	// 开放编码优化是把 defer 函数展开，插入到了函数末尾执行
	openDefer bool
	sp        uintptr // sp at time of defer
	pc        uintptr // pc at time of defer
	// fn 是 defer 关键字中传入的函数
	fn     func()  // can be nil for open-coded defers
	_panic *_panic // panic that is running defer
	link   *_defer // next defer on G; can point to either heap or stack!

	// If openDefer is true, the fields below record values about the stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // funcdata for the function associated with the frame
	varp uintptr        // value of varp for the stack frame
	// framepc is the current pc associated with the stack frame. Together,
	// with sp above (which is the sp associated with the stack frame),
	// framepc/sp can be used as pc/sp pair to continue a stack trace via
	// gentraceback().
	framepc uintptr
}

// A _panic holds information about an active panic.
//
// A _panic value must only ever live on the stack.
//
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
type _panic struct {
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       any            // argument to panic
	link      *_panic        // link to earlier panic
	pc        uintptr        // where to return to in runtime if this panic is bypassed
	sp        unsafe.Pointer // where to return to in runtime if this panic is bypassed
	recovered bool           // whether this panic is over
	aborted   bool           // the panic was aborted
	goexit    bool
}

// stack traces
type stkframe struct {
	fn       funcInfo   // function being run
	pc       uintptr    // program counter within fn
	continpc uintptr    // program counter where execution can continue, or 0 if not
	lr       uintptr    // program counter at caller aka link register
	sp       uintptr    // stack pointer at pc
	fp       uintptr    // stack pointer at caller aka frame pointer
	varp     uintptr    // top of local variables
	argp     uintptr    // pointer to function arguments
	arglen   uintptr    // number of bytes at argp
	argmap   *bitvector // force use of this argmap
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid int64     // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonTimerGoroutineIdle                      // "timer goroutine (idle)"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonTimerGoroutineIdle:    "timer goroutine (idle)",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

var (
	// 保存所有的 m
	allm *m

	// p 的最大值，默认等于 ncpu
	gomaxprocs int32
	// 程序启动时，会调用 osinit 函数获得此值
	ncpu     int32
	forcegc  forcegcstate
	sched    schedt
	newprocs int32

	// allpLock protects P-less reads and size changes of allp, idlepMask,
	// and timerpMask, and all writes to allp.
	allpLock mutex
	// len(allp) == gomaxprocs; may change at safe points, otherwise
	// immutable.
	// 所有的 P 都会存在这个数组里
	allp []*p
	// Bitmask of Ps in _Pidle list, one bit per P. Reads and writes must
	// be atomic. Length may change at safe points.
	//
	// Each P must update only its own bit. In order to maintain
	// consistency, a P going idle must the idle mask simultaneously with
	// updates to the idle P list under the sched.lock, otherwise a racing
	// pidleget may clear the mask before pidleput sets the mask,
	// corrupting the bitmap.
	//
	// N.B., procresize takes ownership of all Ps in stopTheWorldWithSema.
	idlepMask pMask
	// Bitmask of Ps that may have a timer, one bit per P. Reads and writes
	// must be atomic. Length may change at safe points.
	timerpMask pMask

	// Pool of GC parked background workers. Entries are type
	// *gcBgMarkWorkerNode.
	gcBgMarkWorkerPool lfstack

	// Total number of gcBgMarkWorker goroutines. Protected by worldsema.
	gcBgMarkWorkerCount int32

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// Set on startup in asm_{386,amd64}.s
	processorVersionInfo uint32
	isIntel              bool

	goarm uint8 // set by cmd/link on arm systems
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)

// Must agree with internal/buildcfg.Experiment.FramePointer.
const framepointer_enabled = GOARCH == "amd64" || GOARCH == "arm64"
