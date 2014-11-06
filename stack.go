// Package stack implements utilities to capture, manipulate, and format call
// stacks. It provides a simpler API than package runtime.
//
// The implementation takes care of the minutia and special cases of
// interpreting the program counter (pc) values returned by runtime.Callers.
//
// Package stack's types implement fmt.Formatter, which provides a simple and
// flexible way to declaratively configure formatting when used with logging
// or error tracking packages.
package stack

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Call records a single function invocation from a goroutine stack.
type Call struct {
	fn *runtime.Func
	pc uintptr
}

// Caller returns a Call from the stack of the current goroutine. The argument
// skip is the number of stack frames to ascend, with 0 identifying the
// calling function.
func Caller(skip int) Call {
	var pcs [2]uintptr
	n := runtime.Callers(skip+1, pcs[:])

	var c Call

	if n < 2 {
		return c
	}

	c.pc = pcs[1]
	if runtime.FuncForPC(pcs[0]) != sigpanic {
		c.pc--
	}
	c.fn = runtime.FuncForPC(c.pc)
	return c
}

// Format implements fmt.Formatter with support for the following verbs.
//
//    %s    source file
//    %d    line number
//    %n    function name
//    %v    equivalent to %s:%d
//
// It accepts the '+' and '#' flags for most of the verbs as follows.
//
//    %+s   path of source file relative to the compile time GOPATH
//    %#s   full path of source file
//    %+n   import path qualified function name
//    %+v   equivalent to %+s:%d
//    %#v   equivalent to %#s:%d
func (c Call) Format(s fmt.State, verb rune) {
	if c.fn == nil {
		fmt.Fprintf(s, "%%!%c(NOFUNC)", verb)
		return
	}

	switch verb {
	case 's', 'v':
		file, line := c.fn.FileLine(uintptr(c.pc))
		switch {
		case s.Flag('#'):
			// done
		case s.Flag('+'):
			// Here we want to get the source file path relative to the
			// compile time GOPATH. As of Go 1.4.x there is no direct way to
			// know the compiled GOPATH at runtime, but we can infer the
			// number of path segments in the GOPATH. We note that fn.Name()
			// returns the function name qualified by the import path, which
			// does not include the GOPATH. Thus we can trim segments from the
			// beginning of the file path until the number of path separators
			// remaining is one more than the number of path separators in the
			// function name. For example, given:
			//
			//    GOPATH     /home/user
			//    file       /home/user/src/pkg/sub/file.go
			//    fn.Name()  pkg/sub.Type.Method
			//
			// We want to produce:
			//
			//    pkg/sub/file.go
			//
			// From this we can easily see that fn.Name() has one less path
			// separator than our desired output.
			const sep = "/"
			impCnt := strings.Count(c.fn.Name(), sep) + 1
			pathCnt := strings.Count(file, sep)
			for pathCnt > impCnt {
				i := strings.Index(file, sep)
				if i == -1 {
					break
				}
				file = file[i+len(sep):]
				pathCnt--
			}
		default:
			const sep = "/"
			if i := strings.LastIndex(file, sep); i != -1 {
				file = file[i+len(sep):]
			}
		}
		fmt.Fprint(s, file)
		if verb == 'v' {
			fmt.Fprint(s, ":", line)
		}

	case 'd':
		_, line := c.fn.FileLine(uintptr(c.pc))
		fmt.Fprint(s, line)

	case 'n':
		name := c.fn.Name()
		if !s.Flag('+') {
			const pathSep = "/"
			if i := strings.LastIndex(name, pathSep); i != -1 {
				name = name[i+len(pathSep):]
			}
			const pkgSep = "."
			if i := strings.Index(name, pkgSep); i != -1 {
				name = name[i+len(pkgSep):]
			}
		}
		fmt.Fprint(s, name)
	}
}

// name returns the import path qualified name of the function containing the
// call.
func (c Call) name() string {
	if c.fn == nil {
		return "???"
	}
	return c.fn.Name()
}

func (c Call) file() string {
	if c.fn == nil {
		return "???"
	}
	file, _ := c.fn.FileLine(uintptr(c.pc))
	return file
}

func (c Call) line() int {
	if c.fn == nil {
		return 0
	}
	_, line := c.fn.FileLine(c.pc)
	return line
}

// CallStack records a sequence of function invocations from a goroutine
// stack.
type CallStack []Call

// Format implements fmt.Formatter by printing the CallStack as square brackes
// ([, ]) surrounding a space separated list of Calls each formatted with the
// supplied verb and options.
func (cs CallStack) Format(s fmt.State, verb rune) {
	s.Write([]byte("["))
	for i, pc := range cs {
		if i > 0 {
			s.Write([]byte(" "))
		}
		pc.Format(s, verb)
	}
	s.Write([]byte("]"))
}

// findSigpanic intentially executes faulting code to generate a stack trace
// containing an entry for runtime.sigpanic.
func findSigpanic() *runtime.Func {
	var fn *runtime.Func
	func() int {
		defer func() {
			if p := recover(); p != nil {
				pcs := pcStackPool.Get().([]uintptr)
				pcs = pcs[:cap(pcs)]
				n := runtime.Callers(2, pcs)
				for _, pc := range pcs[:n] {
					f := runtime.FuncForPC(pc)
					if f.Name() == "runtime.sigpanic" {
						fn = f
						break
					}
				}
				pcStackPool.Put(pcs)
			}
		}()
		// intentional division by zero fault
		a, b := 1, 0
		return a / b
	}()
	return fn
}

var (
	sigpanic *runtime.Func
	spOnce   sync.Once
)

var pcStackPool = sync.Pool{
	New: func() interface{} { return make([]uintptr, 1000) },
}

// Trace returns a CallStack for the current goroutine with element 0
// identifying the calling function.
func Trace() CallStack {
	spOnce.Do(func() {
		sigpanic = findSigpanic()
	})

	pcs := pcStackPool.Get().([]uintptr)
	pcs = pcs[:cap(pcs)]

	n := runtime.Callers(2, pcs)
	cs := make([]Call, n)

	for i, pc := range pcs[:n] {
		pcFix := pc
		if i > 0 && cs[i-1].fn != sigpanic {
			pcFix--
		}
		cs[i] = Call{
			fn: runtime.FuncForPC(pcFix),
			pc: pcFix,
		}
	}

	pcStackPool.Put(pcs)

	return cs
}

// TrimBelow returns a slice of the CallStack with all entries below c
// removed.
func (cs CallStack) TrimBelow(c Call) CallStack {
	for len(cs) > 0 && cs[0].pc != c.pc {
		cs = cs[1:]
	}
	return cs
}

// TrimAbove returns a slice of the CallStack with all entries above c
// removed.
func (cs CallStack) TrimAbove(c Call) CallStack {
	for len(cs) > 0 && cs[len(cs)-1].pc != c.pc {
		cs = cs[:len(cs)-1]
	}
	return cs
}

var goroot string

func init() {
	goroot = filepath.ToSlash(runtime.GOROOT())
	if runtime.GOOS == "windows" {
		goroot = strings.ToLower(goroot)
	}
}

func inGoroot(path string) bool {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return strings.HasPrefix(path, goroot)
}

// TrimRuntime returns a slice of the CallStack with the topmost entries from
// the go runtime removed. It considers any calls originating from files under
// GOROOT as part of the runtime.
func (cs CallStack) TrimRuntime() CallStack {
	for len(cs) > 0 && inGoroot(cs[len(cs)-1].file()) {
		cs = cs[:len(cs)-1]
	}
	return cs
}
