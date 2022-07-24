// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build amd64 || 386

package runtime

import (
	"internal/goarch"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then stopped before the first instruction in fn.
// gostartcall函数的主要作用有两个：
//    1. 调整 newg 的栈空间，把 goexit 函数的第二条指令的地址入栈，伪造成 goexit 函数调用了 fn，从而使 fn 执行完成后执行 ret 指令时返回到 goexit 继续执行完成最后的清理工作；
//    2. 重新设置 newg.buf.pc 为需要执行的函数的地址，即 fn，初始化时为 runtime.main 函数的地址。
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	// newg 的栈顶，目前 newg 栈上只有 fn 函数的参数，sp 指向的是 fn 的第一参数
	sp := buf.sp
	// 为返回地址预留空间
	sp -= goarch.PtrSize
	// 这里填的是 newproc1 函数里设置的 goexit 函数的第二条指令
	// 伪装 fn 是被 goexit 函数调用的，使得 fn 执行完后返回到 goexit 继续执行，从而完成清理工作
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc
	// 重新设置 buf.sp
	buf.sp = sp
	// 当 goroutine 被调度起来执行时，会从这里的 pc 值开始执行，初始化时就是 runtime.main
	buf.pc = uintptr(fn)
	buf.ctxt = ctxt
}
