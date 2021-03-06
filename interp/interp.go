package interp

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"go/scanner"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Interpreter node structure for AST and CFG.
type node struct {
	child  []*node        // child subtrees (AST)
	anc    *node          // ancestor (AST)
	start  *node          // entry point in subtree (CFG)
	tnext  *node          // true branch successor (CFG)
	fnext  *node          // false branch successor (CFG)
	interp *Interpreter   // interpreter context
	frame  *frame         // frame pointer used for closures only (TODO: suppress this)
	index  int64          // node index (dot display)
	findex int            // index of value in frame or frame size (func def, type def)
	level  int            // number of frame indirections to access value
	nleft  int            // number of children in left part (assign) or indicates preceding type (compositeLit)
	nright int            // number of children in right part (assign)
	kind   nkind          // kind of node
	pos    token.Pos      // position in source code, relative to fset
	sym    *symbol        // associated symbol
	typ    *itype         // type of value in frame, or nil
	recv   *receiver      // method receiver node for call, or nil
	types  []reflect.Type // frame types, used by function literals only
	action action         // action
	exec   bltn           // generated function to execute
	gen    bltnGenerator  // generator function to produce above bltn
	val    interface{}    // static generic value (CFG execution)
	rval   reflect.Value  // reflection value to let runtime access interpreter (CFG)
	ident  string         // set if node is a var or func
}

// receiver stores method receiver object access path.
type receiver struct {
	node  *node         // receiver value for alias and struct types
	val   reflect.Value // receiver value for interface type and value type
	index []int         // path in receiver value for interface or value type
}

// frame contains values for the current execution level (a function context).
type frame struct {
	// id is an atomic counter used for cancellation, only access
	// via newFrame/runid/setrunid/clone.
	// Located at start of struct to ensure proper aligment.
	id uint64

	anc  *frame          // ancestor frame (global space)
	data []reflect.Value // values

	mutex     sync.RWMutex
	deferred  [][]reflect.Value  // defer stack
	recovered interface{}        // to handle panic recover
	done      reflect.SelectCase // for cancellation of channel operations
}

func newFrame(anc *frame, len int, id uint64) *frame {
	f := &frame{
		anc:  anc,
		data: make([]reflect.Value, len),
		id:   id,
	}
	if anc != nil {
		f.done = anc.done
	}
	return f
}

func (f *frame) runid() uint64      { return atomic.LoadUint64(&f.id) }
func (f *frame) setrunid(id uint64) { atomic.StoreUint64(&f.id, id) }
func (f *frame) clone() *frame {
	f.mutex.RLock()
	defer f.mutex.RUnlock()
	return &frame{
		anc:       f.anc,
		data:      f.data,
		deferred:  f.deferred,
		recovered: f.recovered,
		id:        f.runid(),
		done:      f.done,
	}
}

// Exports stores the map of binary packages per package path.
type Exports map[string]map[string]reflect.Value

// imports stores the map of source packages per package path.
type imports map[string]map[string]*symbol

// opt stores interpreter options.
type opt struct {
	astDot bool // display AST graph (debug)
	cfgDot bool // display CFG graph (debug)
	// dotCmd is the command to process the dot graph produced when astDot and/or
	// cfgDot is enabled. It defaults to 'dot -Tdot -o <filename>.dot'.
	dotCmd   string
	noRun    bool          // compile, but do not run
	fastChan bool          // disable cancellable chan operations
	context  build.Context // build context: GOPATH, build constraints
	stdin    io.Reader     // standard input
	stdout   io.Writer     // standard output
	stderr   io.Writer     // standard error
}

// Interpreter contains global resources and state.
type Interpreter struct {
	// id is an atomic counter counter used for run cancellation,
	// only accessed via runid/stop
	// Located at start of struct to ensure proper alignment on 32 bit
	// architectures.
	id uint64

	name string // name of the input source file (or main)

	opt                        // user settable options
	cancelChan bool            // enables cancellable chan operations
	nindex     int64           // next node index
	fset       *token.FileSet  // fileset to locate node in source code
	binPkg     Exports         // binary packages used in interpreter, indexed by path
	rdir       map[string]bool // for src import cycle detection

	mutex    sync.RWMutex
	frame    *frame            // program data storage during execution
	universe *scope            // interpreter global level scope
	scopes   map[string]*scope // package level scopes, indexed by import path
	srcPkg   imports           // source packages used in interpreter, indexed by path
	pkgNames map[string]string // package names, indexed by import path
	done     chan struct{}     // for cancellation of channel operations

	hooks *hooks // symbol hooks
}

const (
	mainID   = "main"
	selfPath = "github.com/containous/yaegi/interp"
	// DefaultSourceName is the name used by default when the name of the input
	// source file has not been specified for an Eval.
	// TODO(mpl): something even more special as a name?
	DefaultSourceName = "_.go"
)

// Symbols exposes interpreter values.
var Symbols = Exports{
	selfPath: map[string]reflect.Value{
		"New": reflect.ValueOf(New),

		"Interpreter": reflect.ValueOf((*Interpreter)(nil)),
		"Options":     reflect.ValueOf((*Options)(nil)),
	},
}

func init() { Symbols[selfPath]["Symbols"] = reflect.ValueOf(Symbols) }

// _error is a wrapper of error interface type.
type _error struct {
	WError func() string
}

func (w _error) Error() string { return w.WError() }

// Panic is an error recovered from a panic call in interpreted code.
type Panic struct {
	// Value is the recovered value of a call to panic.
	Value interface{}

	// Callers is the call stack obtained from the recover call.
	// It may be used as the parameter to runtime.CallersFrames.
	Callers []uintptr

	// Stack is the call stack buffer for debug.
	Stack []byte
}

// TODO: Capture interpreter stack frames also and remove
// fmt.Println(n.cfgErrorf("panic")) in runCfg.

func (e Panic) Error() string { return fmt.Sprint(e.Value) }

// Walk traverses AST n in depth first order, call cbin function
// at node entry and cbout function at node exit.
func (n *node) Walk(in func(n *node) bool, out func(n *node)) {
	if in != nil && !in(n) {
		return
	}
	for _, child := range n.child {
		child.Walk(in, out)
	}
	if out != nil {
		out(n)
	}
}

// Options are the interpreter options.
type Options struct {
	// GoPath sets GOPATH for the interpreter.
	GoPath string

	// BuildTags sets build constraints for the interpreter.
	BuildTags []string

	// Standard input, output and error streams.
	// They default to os.Stding, os.Stdout and os.Stderr respectively.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// New returns a new interpreter.
func New(options Options) *Interpreter {
	i := Interpreter{
		opt:      opt{context: build.Default},
		frame:    &frame{data: []reflect.Value{}},
		fset:     token.NewFileSet(),
		universe: initUniverse(),
		scopes:   map[string]*scope{},
		binPkg:   Exports{"": map[string]reflect.Value{"_error": reflect.ValueOf((*_error)(nil))}},
		srcPkg:   imports{},
		pkgNames: map[string]string{},
		rdir:     map[string]bool{},
		hooks:    &hooks{},
	}

	if i.opt.stdin = options.Stdin; i.opt.stdin == nil {
		i.opt.stdin = os.Stdin
	}

	if i.opt.stdout = options.Stdout; i.opt.stdout == nil {
		i.opt.stdout = os.Stdout
	}

	if i.opt.stderr = options.Stderr; i.opt.stderr == nil {
		i.opt.stderr = os.Stderr
	}

	i.opt.context.GOPATH = options.GoPath
	if len(options.BuildTags) > 0 {
		i.opt.context.BuildTags = options.BuildTags
	}

	// astDot activates AST graph display for the interpreter
	i.opt.astDot, _ = strconv.ParseBool(os.Getenv("YAEGI_AST_DOT"))

	// cfgDot activates CFG graph display for the interpreter
	i.opt.cfgDot, _ = strconv.ParseBool(os.Getenv("YAEGI_CFG_DOT"))

	// dotCmd defines how to process the dot code generated whenever astDot and/or
	// cfgDot is enabled. It defaults to 'dot -Tdot -o<filename>.dot' where filename
	// is context dependent.
	i.opt.dotCmd = os.Getenv("YAEGI_DOT_CMD")

	// noRun disables the execution (but not the compilation) in the interpreter
	i.opt.noRun, _ = strconv.ParseBool(os.Getenv("YAEGI_NO_RUN"))

	// fastChan disables the cancellable version of channel operations in evalWithContext
	i.opt.fastChan, _ = strconv.ParseBool(os.Getenv("YAEGI_FAST_CHAN"))
	return &i
}

const (
	bltnAppend  = "append"
	bltnCap     = "cap"
	bltnClose   = "close"
	bltnComplex = "complex"
	bltnImag    = "imag"
	bltnCopy    = "copy"
	bltnDelete  = "delete"
	bltnLen     = "len"
	bltnMake    = "make"
	bltnNew     = "new"
	bltnPanic   = "panic"
	bltnPrint   = "print"
	bltnPrintln = "println"
	bltnReal    = "real"
	bltnRecover = "recover"
)

func initUniverse() *scope {
	sc := &scope{global: true, sym: map[string]*symbol{
		// predefined Go types
		"bool":        {kind: typeSym, typ: &itype{cat: boolT, name: "bool"}},
		"byte":        {kind: typeSym, typ: &itype{cat: uint8T, name: "uint8"}},
		"complex64":   {kind: typeSym, typ: &itype{cat: complex64T, name: "complex64"}},
		"complex128":  {kind: typeSym, typ: &itype{cat: complex128T, name: "complex128"}},
		"error":       {kind: typeSym, typ: &itype{cat: errorT, name: "error"}},
		"float32":     {kind: typeSym, typ: &itype{cat: float32T, name: "float32"}},
		"float64":     {kind: typeSym, typ: &itype{cat: float64T, name: "float64"}},
		"int":         {kind: typeSym, typ: &itype{cat: intT, name: "int"}},
		"int8":        {kind: typeSym, typ: &itype{cat: int8T, name: "int8"}},
		"int16":       {kind: typeSym, typ: &itype{cat: int16T, name: "int16"}},
		"int32":       {kind: typeSym, typ: &itype{cat: int32T, name: "int32"}},
		"int64":       {kind: typeSym, typ: &itype{cat: int64T, name: "int64"}},
		"interface{}": {kind: typeSym, typ: &itype{cat: interfaceT}},
		"rune":        {kind: typeSym, typ: &itype{cat: int32T, name: "int32"}},
		"string":      {kind: typeSym, typ: &itype{cat: stringT, name: "string"}},
		"uint":        {kind: typeSym, typ: &itype{cat: uintT, name: "uint"}},
		"uint8":       {kind: typeSym, typ: &itype{cat: uint8T, name: "uint8"}},
		"uint16":      {kind: typeSym, typ: &itype{cat: uint16T, name: "uint16"}},
		"uint32":      {kind: typeSym, typ: &itype{cat: uint32T, name: "uint32"}},
		"uint64":      {kind: typeSym, typ: &itype{cat: uint64T, name: "uint64"}},
		"uintptr":     {kind: typeSym, typ: &itype{cat: uintptrT, name: "uintptr"}},

		// predefined Go constants
		"false": {kind: constSym, typ: untypedBool(), rval: reflect.ValueOf(false)},
		"true":  {kind: constSym, typ: untypedBool(), rval: reflect.ValueOf(true)},
		"iota":  {kind: constSym, typ: untypedInt()},

		// predefined Go zero value
		"nil": {typ: &itype{cat: nilT, untyped: true}},

		// predefined Go builtins
		bltnAppend:  {kind: bltnSym, builtin: _append},
		bltnCap:     {kind: bltnSym, builtin: _cap},
		bltnClose:   {kind: bltnSym, builtin: _close},
		bltnComplex: {kind: bltnSym, builtin: _complex},
		bltnImag:    {kind: bltnSym, builtin: _imag},
		bltnCopy:    {kind: bltnSym, builtin: _copy},
		bltnDelete:  {kind: bltnSym, builtin: _delete},
		bltnLen:     {kind: bltnSym, builtin: _len},
		bltnMake:    {kind: bltnSym, builtin: _make},
		bltnNew:     {kind: bltnSym, builtin: _new},
		bltnPanic:   {kind: bltnSym, builtin: _panic},
		bltnPrint:   {kind: bltnSym, builtin: _print},
		bltnPrintln: {kind: bltnSym, builtin: _println},
		bltnReal:    {kind: bltnSym, builtin: _real},
		bltnRecover: {kind: bltnSym, builtin: _recover},
	}}
	return sc
}

// resizeFrame resizes the global frame of interpreter.
func (interp *Interpreter) resizeFrame() {
	l := len(interp.universe.types)
	b := len(interp.frame.data)
	if l-b <= 0 {
		return
	}
	data := make([]reflect.Value, l)
	copy(data, interp.frame.data)
	for j, t := range interp.universe.types[b:] {
		data[b+j] = reflect.New(t).Elem()
	}
	interp.frame.data = data
}

func (interp *Interpreter) main() *node {
	interp.mutex.RLock()
	defer interp.mutex.RUnlock()
	if m, ok := interp.scopes[mainID]; ok && m.sym[mainID] != nil {
		return m.sym[mainID].node
	}
	return nil
}

// Eval evaluates Go code represented as a string. Eval returns the last result
// computed by the interpreter, and a non nil error in case of failure.
func (interp *Interpreter) Eval(src string) (res reflect.Value, err error) {
	return interp.eval(src, "", true)
}

// EvalPath evaluates Go code located at path. EvalPath returns the last result
// computed by the interpreter, and a non nil error in case of failure.
func (interp *Interpreter) EvalPath(path string) (res reflect.Value, err error) {
	// TODO(marc): implement eval of a directory, package and tests.
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return res, err
	}
	return interp.eval(string(b), path, false)
}

func (interp *Interpreter) eval(src, name string, inc bool) (res reflect.Value, err error) {
	if name != "" {
		interp.name = name
	}
	if interp.name == "" {
		interp.name = DefaultSourceName
	}

	defer func() {
		r := recover()
		if r != nil {
			var pc [64]uintptr // 64 frames should be enough.
			n := runtime.Callers(1, pc[:])
			err = Panic{Value: r, Callers: pc[:n], Stack: debug.Stack()}
		}
	}()

	// Parse source to AST.
	pkgName, root, err := interp.ast(src, interp.name, inc)
	if err != nil || root == nil {
		return res, err
	}

	if interp.astDot {
		dotCmd := interp.dotCmd
		if dotCmd == "" {
			dotCmd = defaultDotCmd(interp.name, "yaegi-ast-")
		}
		root.astDot(dotWriter(dotCmd), interp.name)
		if interp.noRun {
			return res, err
		}
	}

	// Perform global types analysis.
	if err = interp.gtaRetry([]*node{root}, pkgName); err != nil {
		return res, err
	}

	// Annotate AST with CFG infos
	initNodes, err := interp.cfg(root, pkgName)
	if err != nil {
		if interp.cfgDot {
			dotCmd := interp.dotCmd
			if dotCmd == "" {
				dotCmd = defaultDotCmd(interp.name, "yaegi-cfg-")
			}
			root.cfgDot(dotWriter(dotCmd))
		}
		return res, err
	}

	// Add main to list of functions to run, after all inits
	if m := interp.main(); m != nil {
		initNodes = append(initNodes, m)
	}

	if root.kind != fileStmt {
		// REPL may skip package statement
		setExec(root.start)
	}
	interp.mutex.Lock()
	if interp.universe.sym[pkgName] == nil {
		// Make the package visible under a path identical to its name
		// TODO(mpl): srcPkg is supposed to be keyed by importPath. Verify it is necessary, and implement.
		interp.srcPkg[pkgName] = interp.scopes[pkgName].sym
		interp.universe.sym[pkgName] = &symbol{kind: pkgSym, typ: &itype{cat: srcPkgT, path: pkgName}}
		interp.pkgNames[pkgName] = pkgName
	}
	interp.mutex.Unlock()

	if interp.cfgDot {
		dotCmd := interp.dotCmd
		if dotCmd == "" {
			dotCmd = defaultDotCmd(interp.name, "yaegi-cfg-")
		}
		root.cfgDot(dotWriter(dotCmd))
	}

	if interp.noRun {
		return res, err
	}

	// Generate node exec closures
	if err = genRun(root); err != nil {
		return res, err
	}

	// Init interpreter execution memory frame
	interp.frame.setrunid(interp.runid())
	interp.frame.mutex.Lock()
	interp.resizeFrame()
	interp.frame.mutex.Unlock()

	// Execute node closures
	interp.run(root, nil)

	// Wire and execute global vars
	n, err := genGlobalVars([]*node{root}, interp.scopes[pkgName])
	if err != nil {
		return res, err
	}
	interp.run(n, nil)

	for _, n := range initNodes {
		interp.run(n, interp.frame)
	}
	v := genValue(root)
	res = v(interp.frame)

	// If result is an interpreter node, wrap it in a runtime callable function
	if res.IsValid() {
		if n, ok := res.Interface().(*node); ok {
			res = genFunctionWrapper(n)(interp.frame)
		}
	}

	return res, err
}

// EvalWithContext evaluates Go code represented as a string. It returns
// a map on current interpreted package exported symbols.
func (interp *Interpreter) EvalWithContext(ctx context.Context, src string) (reflect.Value, error) {
	var v reflect.Value
	var err error

	interp.mutex.Lock()
	interp.done = make(chan struct{})
	interp.cancelChan = !interp.opt.fastChan
	interp.mutex.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		v, err = interp.Eval(src)
	}()

	select {
	case <-ctx.Done():
		interp.stop()
		return reflect.Value{}, ctx.Err()
	case <-done:
	}
	return v, err
}

// stop sends a semaphore to all running frames and closes the chan
// operation short circuit channel. stop may only be called once per
// invocation of EvalWithContext.
func (interp *Interpreter) stop() {
	atomic.AddUint64(&interp.id, 1)
	close(interp.done)
}

func (interp *Interpreter) runid() uint64 { return atomic.LoadUint64(&interp.id) }

// getWrapper returns the wrapper type of the corresponding interface, or nil if not found.
func (interp *Interpreter) getWrapper(t reflect.Type) reflect.Type {
	if p, ok := interp.binPkg[t.PkgPath()]; ok {
		return p["_"+t.Name()].Type().Elem()
	}
	return nil
}

// Use loads binary runtime symbols in the interpreter context so
// they can be used in interpreted code.
func (interp *Interpreter) Use(values Exports) {
	for k, v := range values {
		if k == hooksPath {
			interp.hooks.Parse(v)
			continue
		}

		if interp.binPkg[k] == nil {
			interp.binPkg[k] = make(map[string]reflect.Value)
		}

		for s, sym := range v {
			interp.binPkg[k][s] = sym
		}
	}

	// Checks if input values correspond to stdlib packages by looking for one
	// well known stdlib package path.
	if _, ok := values["fmt"]; ok {
		fixStdio(interp)
	}
}

// fixStdio redefines interpreter stdlib symbols to use the standard input,
// output and errror assigned to the interpreter. The changes are limited to
// the interpreter only. Global values os.Stdin, os.Stdout and os.Stderr are
// not changed. Note that it is possible to escape the virtualized stdio by
// read/write directly to file descriptors 0, 1, 2.
func fixStdio(interp *Interpreter) {
	p := interp.binPkg["fmt"]
	if p == nil {
		return
	}

	stdin, stdout, stderr := interp.stdin, interp.stdout, interp.stderr

	p["Print"] = reflect.ValueOf(func(a ...interface{}) (n int, err error) { return fmt.Fprint(stdout, a...) })
	p["Printf"] = reflect.ValueOf(func(f string, a ...interface{}) (n int, err error) { return fmt.Fprintf(stdout, f, a...) })
	p["Println"] = reflect.ValueOf(func(a ...interface{}) (n int, err error) { return fmt.Fprintln(stdout, a...) })

	p["Scan"] = reflect.ValueOf(func(a ...interface{}) (n int, err error) { return fmt.Fscan(stdin, a...) })
	p["Scanf"] = reflect.ValueOf(func(f string, a ...interface{}) (n int, err error) { return fmt.Fscanf(stdin, f, a...) })
	p["Scanln"] = reflect.ValueOf(func(a ...interface{}) (n int, err error) { return fmt.Fscanln(stdin, a...) })

	if p = interp.binPkg["flag"]; p != nil {
		c := flag.NewFlagSet(os.Args[0], flag.PanicOnError)
		c.SetOutput(stderr)
		p["CommandLine"] = reflect.ValueOf(&c).Elem()
	}

	if p = interp.binPkg["log"]; p != nil {
		l := log.New(stderr, "", log.LstdFlags)
		// Restrict Fatal symbols to panic instead of exit.
		p["Fatal"] = reflect.ValueOf(l.Panic)
		p["Fatalf"] = reflect.ValueOf(l.Panicf)
		p["Fatalln"] = reflect.ValueOf(l.Panicln)

		p["Flags"] = reflect.ValueOf(l.Flags)
		p["Output"] = reflect.ValueOf(l.Output)
		p["Panic"] = reflect.ValueOf(l.Panic)
		p["Panicf"] = reflect.ValueOf(l.Panicf)
		p["Panicln"] = reflect.ValueOf(l.Panicln)
		p["Prefix"] = reflect.ValueOf(l.Prefix)
		p["Print"] = reflect.ValueOf(l.Print)
		p["Printf"] = reflect.ValueOf(l.Printf)
		p["Println"] = reflect.ValueOf(l.Println)
		p["SetFlags"] = reflect.ValueOf(l.SetFlags)
		p["SetOutput"] = reflect.ValueOf(l.SetOutput)
		p["SetPrefix"] = reflect.ValueOf(l.SetPrefix)
		p["Writer"] = reflect.ValueOf(l.Writer)
	}

	if p = interp.binPkg["os"]; p != nil {
		p["Stdin"] = reflect.ValueOf(&stdin).Elem()
		p["Stdout"] = reflect.ValueOf(&stdout).Elem()
		p["Stderr"] = reflect.ValueOf(&stderr).Elem()
	}
}

// ignoreScannerError returns true if the error from Go scanner can be safely ignored
// to let the caller grab one more line before retrying to parse its input.
func ignoreScannerError(e *scanner.Error, s string) bool {
	msg := e.Msg
	if strings.HasSuffix(msg, "found 'EOF'") {
		return true
	}
	if msg == "raw string literal not terminated" {
		return true
	}
	if strings.HasPrefix(msg, "expected operand, found '}'") && !strings.HasSuffix(s, "}") {
		return true
	}
	return false
}

// REPL performs a Read-Eval-Print-Loop on input reader.
// Results are printed to the output writer of the Interpreter, provided as option
// at creation time. Errors are printed to the similarly defined errors writer.
// The last interpreter result value and error are returned.
func (interp *Interpreter) REPL() (reflect.Value, error) {
	// Preimport used bin packages, to avoid having to import these packages manually
	// in REPL mode. These packages are already loaded anyway.
	sc := interp.universe
	for k := range interp.binPkg {
		name := identifier.FindString(k)
		if name == "" || name == "rand" || name == "scanner" || name == "template" || name == "pprof" {
			// Skip any package with an ambiguous name (i.e crypto/rand vs math/rand).
			// Those will have to be imported explicitly.
			continue
		}
		sc.sym[name] = &symbol{kind: pkgSym, typ: &itype{cat: binPkgT, path: k, scope: sc}}
	}

	in, out, errs := interp.stdin, interp.stdout, interp.stderr
	ctx, cancel := context.WithCancel(context.Background())
	end := make(chan struct{})     // channel to terminate the REPL
	sig := make(chan os.Signal, 1) // channel to trap interrupt signal (Ctrl-C)
	lines := make(chan string)     // channel to read REPL input lines
	prompt := getPrompt(in, out)   // prompt activated on tty like IO stream
	s := bufio.NewScanner(in)      // read input stream line by line
	var v reflect.Value            // result value from eval
	var err error                  // error from eval
	src := ""                      // source string to evaluate

	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	prompt(v)

	go func() {
		defer close(end)
		for s.Scan() {
			lines <- s.Text()
		}
		if e := s.Err(); e != nil {
			fmt.Fprintln(errs, e)
		}
	}()

	go func() {
		for {
			select {
			case <-sig:
				cancel()
				lines <- ""
			case <-end:
				return
			}
		}
	}()

	for {
		var line string

		select {
		case <-end:
			cancel()
			return v, err
		case line = <-lines:
			src += line + "\n"
		}

		v, err = interp.EvalWithContext(ctx, src)
		if err != nil {
			switch e := err.(type) {
			case scanner.ErrorList:
				if len(e) > 0 && ignoreScannerError(e[0], line) {
					continue
				}
				fmt.Fprintln(errs, strings.TrimPrefix(e[0].Error(), DefaultSourceName+":"))
			case Panic:
				fmt.Fprintln(errs, e.Value)
				fmt.Fprintln(errs, string(e.Stack))
			default:
				fmt.Fprintln(errs, err)
			}
		}
		if errors.Is(err, context.Canceled) {
			ctx, cancel = context.WithCancel(context.Background())
		}
		src = ""
		prompt(v)
	}
}

// getPrompt returns a function which prints a prompt only if input is a terminal.
func getPrompt(in io.Reader, out io.Writer) func(reflect.Value) {
	s, ok := in.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return func(reflect.Value) {}
	}
	stat, err := s.Stat()
	if err == nil && stat.Mode()&os.ModeCharDevice != 0 {
		return func(v reflect.Value) {
			if v.IsValid() {
				fmt.Fprintln(out, ":", v)
			}
			fmt.Fprint(out, "> ")
		}
	}
	return func(reflect.Value) {}
}
