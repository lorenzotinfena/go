// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package staticinit

import (
	"fmt"
	"go/constant"
	"go/token"

	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/reflectdata"
	"cmd/compile/internal/staticdata"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"cmd/internal/obj"
	"cmd/internal/src"
)

type Entry struct {
	Xoffset int64   // struct, array only
	Expr    ir.Node // bytes of run-time computed expressions
}

type Plan struct {
	E []Entry
}

// An Schedule is used to decompose assignment statements into
// static and dynamic initialization parts. Static initializations are
// handled by populating variables' linker symbol data, while dynamic
// initializations are accumulated to be executed in order.
type Schedule struct {
	// Out is the ordered list of dynamic initialization
	// statements.
	Out []ir.Node

	Plans map[ir.Node]*Plan
	Temps map[ir.Node]*ir.Name
}

func (s *Schedule) append(n ir.Node) {
	s.Out = append(s.Out, n)
}

// StaticInit adds an initialization statement n to the schedule.
func (s *Schedule) StaticInit(n ir.Node) {
	if !s.tryStaticInit(n) {
		if base.Flag.Percent != 0 {
			ir.Dump("StaticInit failed", n)
		}
		s.append(n)
	}
}

// tryStaticInit attempts to statically execute an initialization
// statement and reports whether it succeeded.
func (s *Schedule) tryStaticInit(nn ir.Node) bool {
	// Only worry about simple "l = r" assignments. Multiple
	// variable/expression OAS2 assignments have already been
	// replaced by multiple simple OAS assignments, and the other
	// OAS2* assignments mostly necessitate dynamic execution
	// anyway.
	if nn.Op() != ir.OAS {
		return false
	}
	n := nn.(*ir.AssignStmt)
	if ir.IsBlank(n.X) && !AnySideEffects(n.Y) {
		// Discard.
		return true
	}
	lno := ir.SetPos(n)
	defer func() { base.Pos = lno }()
	nam := n.X.(*ir.Name)
	return s.StaticAssign(nam, 0, n.Y, nam.Type())
}

// like staticassign but we are copying an already
// initialized value r.
func (s *Schedule) staticcopy(l *ir.Name, loff int64, rn *ir.Name, typ *types.Type) bool {
	if rn.Class == ir.PFUNC {
		// TODO if roff != 0 { panic }
		staticdata.InitAddr(l, loff, staticdata.FuncLinksym(rn))
		return true
	}
	if rn.Class != ir.PEXTERN || rn.Sym().Pkg != types.LocalPkg {
		return false
	}
	if rn.Defn.Op() != ir.OAS {
		return false
	}
	if rn.Type().IsString() { // perhaps overwritten by cmd/link -X (#34675)
		return false
	}
	if rn.Embed != nil {
		return false
	}
	orig := rn
	r := rn.Defn.(*ir.AssignStmt).Y
	if r == nil {
		// No explicit initialization value. Probably zeroed but perhaps
		// supplied externally and of unknown value.
		return false
	}

	for r.Op() == ir.OCONVNOP && !types.Identical(r.Type(), typ) {
		r = r.(*ir.ConvExpr).X
	}

	switch r.Op() {
	case ir.OMETHEXPR:
		r = r.(*ir.SelectorExpr).FuncName()
		fallthrough
	case ir.ONAME:
		r := r.(*ir.Name)
		if s.staticcopy(l, loff, r, typ) {
			return true
		}
		// We may have skipped past one or more OCONVNOPs, so
		// use conv to ensure r is assignable to l (#13263).
		dst := ir.Node(l)
		if loff != 0 || !types.Identical(typ, l.Type()) {
			dst = ir.NewNameOffsetExpr(base.Pos, l, loff, typ)
		}
		s.append(ir.NewAssignStmt(base.Pos, dst, typecheck.Conv(r, typ)))
		return true

	case ir.ONIL:
		return true

	case ir.OLITERAL:
		if ir.IsZero(r) {
			return true
		}
		staticdata.InitConst(l, loff, r, int(typ.Size()))
		return true

	case ir.OADDR:
		r := r.(*ir.AddrExpr)
		if a, ok := r.X.(*ir.Name); ok && a.Op() == ir.ONAME {
			staticdata.InitAddr(l, loff, staticdata.GlobalLinksym(a))
			return true
		}

	case ir.OPTRLIT:
		r := r.(*ir.AddrExpr)
		switch r.X.Op() {
		case ir.OARRAYLIT, ir.OSLICELIT, ir.OSTRUCTLIT, ir.OMAPLIT:
			// copy pointer
			staticdata.InitAddr(l, loff, staticdata.GlobalLinksym(s.Temps[r]))
			return true
		}

	case ir.OSLICELIT:
		r := r.(*ir.CompLitExpr)
		// copy slice
		staticdata.InitSlice(l, loff, staticdata.GlobalLinksym(s.Temps[r]), r.Len)
		return true

	case ir.OARRAYLIT, ir.OSTRUCTLIT:
		r := r.(*ir.CompLitExpr)
		p := s.Plans[r]
		for i := range p.E {
			e := &p.E[i]
			typ := e.Expr.Type()
			if e.Expr.Op() == ir.OLITERAL || e.Expr.Op() == ir.ONIL {
				staticdata.InitConst(l, loff+e.Xoffset, e.Expr, int(typ.Size()))
				continue
			}
			x := e.Expr
			if x.Op() == ir.OMETHEXPR {
				x = x.(*ir.SelectorExpr).FuncName()
			}
			if x.Op() == ir.ONAME && s.staticcopy(l, loff+e.Xoffset, x.(*ir.Name), typ) {
				continue
			}
			// Requires computation, but we're
			// copying someone else's computation.
			ll := ir.NewNameOffsetExpr(base.Pos, l, loff+e.Xoffset, typ)
			rr := ir.NewNameOffsetExpr(base.Pos, orig, e.Xoffset, typ)
			ir.SetPos(rr)
			s.append(ir.NewAssignStmt(base.Pos, ll, rr))
		}

		return true
	}

	return false
}

func (s *Schedule) StaticAssign(l *ir.Name, loff int64, r ir.Node, typ *types.Type) bool {
	if r == nil {
		// No explicit initialization value. Either zero or supplied
		// externally.
		return true
	}
	for r.Op() == ir.OCONVNOP {
		r = r.(*ir.ConvExpr).X
	}

	assign := func(pos src.XPos, a *ir.Name, aoff int64, v ir.Node) {
		if s.StaticAssign(a, aoff, v, v.Type()) {
			return
		}
		var lhs ir.Node
		if ir.IsBlank(a) {
			// Don't use NameOffsetExpr with blank (#43677).
			lhs = ir.BlankNode
		} else {
			lhs = ir.NewNameOffsetExpr(pos, a, aoff, v.Type())
		}
		s.append(ir.NewAssignStmt(pos, lhs, v))
	}

	switch r.Op() {
	case ir.ONAME:
		r := r.(*ir.Name)
		return s.staticcopy(l, loff, r, typ)

	case ir.OMETHEXPR:
		r := r.(*ir.SelectorExpr)
		return s.staticcopy(l, loff, r.FuncName(), typ)

	case ir.ONIL:
		return true

	case ir.OLITERAL:
		if ir.IsZero(r) {
			return true
		}
		staticdata.InitConst(l, loff, r, int(typ.Size()))
		return true

	case ir.OADDR:
		r := r.(*ir.AddrExpr)
		if name, offset, ok := StaticLoc(r.X); ok && name.Class == ir.PEXTERN {
			staticdata.InitAddrOffset(l, loff, name.Linksym(), offset)
			return true
		}
		fallthrough

	case ir.OPTRLIT:
		r := r.(*ir.AddrExpr)
		switch r.X.Op() {
		case ir.OARRAYLIT, ir.OSLICELIT, ir.OMAPLIT, ir.OSTRUCTLIT:
			// Init pointer.
			a := StaticName(r.X.Type())

			s.Temps[r] = a
			staticdata.InitAddr(l, loff, a.Linksym())

			// Init underlying literal.
			assign(base.Pos, a, 0, r.X)
			return true
		}
		//dump("not static ptrlit", r);

	case ir.OSTR2BYTES:
		r := r.(*ir.ConvExpr)
		if l.Class == ir.PEXTERN && r.X.Op() == ir.OLITERAL {
			sval := ir.StringVal(r.X)
			staticdata.InitSliceBytes(l, loff, sval)
			return true
		}

	case ir.OSLICELIT:
		r := r.(*ir.CompLitExpr)
		s.initplan(r)
		// Init slice.
		ta := types.NewArray(r.Type().Elem(), r.Len)
		ta.SetNoalg(true)
		a := StaticName(ta)
		s.Temps[r] = a
		staticdata.InitSlice(l, loff, a.Linksym(), r.Len)
		// Fall through to init underlying array.
		l = a
		loff = 0
		fallthrough

	case ir.OARRAYLIT, ir.OSTRUCTLIT:
		r := r.(*ir.CompLitExpr)
		s.initplan(r)

		p := s.Plans[r]
		for i := range p.E {
			e := &p.E[i]
			if e.Expr.Op() == ir.OLITERAL || e.Expr.Op() == ir.ONIL {
				staticdata.InitConst(l, loff+e.Xoffset, e.Expr, int(e.Expr.Type().Size()))
				continue
			}
			ir.SetPos(e.Expr)
			assign(base.Pos, l, loff+e.Xoffset, e.Expr)
		}

		return true

	case ir.OMAPLIT:
		break

	case ir.OCLOSURE:
		r := r.(*ir.ClosureExpr)
		if ir.IsTrivialClosure(r) {
			if base.Debug.Closure > 0 {
				base.WarnfAt(r.Pos(), "closure converted to global")
			}
			// Closures with no captured variables are globals,
			// so the assignment can be done at link time.
			// TODO if roff != 0 { panic }
			staticdata.InitAddr(l, loff, staticdata.FuncLinksym(r.Func.Nname))
			return true
		}
		ir.ClosureDebugRuntimeCheck(r)

	case ir.OCONVIFACE:
		// This logic is mirrored in isStaticCompositeLiteral.
		// If you change something here, change it there, and vice versa.

		// Determine the underlying concrete type and value we are converting from.
		r := r.(*ir.ConvExpr)
		val := ir.Node(r)
		for val.Op() == ir.OCONVIFACE {
			val = val.(*ir.ConvExpr).X
		}

		if val.Type().IsInterface() {
			// val is an interface type.
			// If val is nil, we can statically initialize l;
			// both words are zero and so there no work to do, so report success.
			// If val is non-nil, we have no concrete type to record,
			// and we won't be able to statically initialize its value, so report failure.
			return val.Op() == ir.ONIL
		}

		reflectdata.MarkTypeUsedInInterface(val.Type(), l.Linksym())

		var itab *ir.AddrExpr
		if typ.IsEmptyInterface() {
			itab = reflectdata.TypePtr(val.Type())
		} else {
			itab = reflectdata.ITabAddr(val.Type(), typ)
		}

		// Create a copy of l to modify while we emit data.

		// Emit itab, advance offset.
		staticdata.InitAddr(l, loff, itab.X.(*ir.LinksymOffsetExpr).Linksym)

		// Emit data.
		if types.IsDirectIface(val.Type()) {
			if val.Op() == ir.ONIL {
				// Nil is zero, nothing to do.
				return true
			}
			// Copy val directly into n.
			ir.SetPos(val)
			assign(base.Pos, l, loff+int64(types.PtrSize), val)
		} else {
			// Construct temp to hold val, write pointer to temp into n.
			a := StaticName(val.Type())
			s.Temps[val] = a
			assign(base.Pos, a, 0, val)
			staticdata.InitAddr(l, loff+int64(types.PtrSize), a.Linksym())
		}

		return true

	case ir.OINLCALL:
		r := r.(*ir.InlinedCallExpr)
		return s.staticAssignInlinedCall(l, loff, r, typ)
	}

	if base.Flag.Percent != 0 {
		ir.Dump("not static", r)
	}
	return false
}

func (s *Schedule) initplan(n ir.Node) {
	if s.Plans[n] != nil {
		return
	}
	p := new(Plan)
	s.Plans[n] = p
	switch n.Op() {
	default:
		base.Fatalf("initplan")

	case ir.OARRAYLIT, ir.OSLICELIT:
		n := n.(*ir.CompLitExpr)
		var k int64
		for _, a := range n.List {
			if a.Op() == ir.OKEY {
				kv := a.(*ir.KeyExpr)
				k = typecheck.IndexConst(kv.Key)
				if k < 0 {
					base.Fatalf("initplan arraylit: invalid index %v", kv.Key)
				}
				a = kv.Value
			}
			s.addvalue(p, k*n.Type().Elem().Size(), a)
			k++
		}

	case ir.OSTRUCTLIT:
		n := n.(*ir.CompLitExpr)
		for _, a := range n.List {
			if a.Op() != ir.OSTRUCTKEY {
				base.Fatalf("initplan structlit")
			}
			a := a.(*ir.StructKeyExpr)
			if a.Sym().IsBlank() {
				continue
			}
			s.addvalue(p, a.Field.Offset, a.Value)
		}

	case ir.OMAPLIT:
		n := n.(*ir.CompLitExpr)
		for _, a := range n.List {
			if a.Op() != ir.OKEY {
				base.Fatalf("initplan maplit")
			}
			a := a.(*ir.KeyExpr)
			s.addvalue(p, -1, a.Value)
		}
	}
}

func (s *Schedule) addvalue(p *Plan, xoffset int64, n ir.Node) {
	// special case: zero can be dropped entirely
	if ir.IsZero(n) {
		return
	}

	// special case: inline struct and array (not slice) literals
	if isvaluelit(n) {
		s.initplan(n)
		q := s.Plans[n]
		for _, qe := range q.E {
			// qe is a copy; we are not modifying entries in q.E
			qe.Xoffset += xoffset
			p.E = append(p.E, qe)
		}
		return
	}

	// add to plan
	p.E = append(p.E, Entry{Xoffset: xoffset, Expr: n})
}

func (s *Schedule) staticAssignInlinedCall(l *ir.Name, loff int64, call *ir.InlinedCallExpr, typ *types.Type) bool {
	// Handle the special case of an inlined call of
	// a function body with a single return statement,
	// which turns into a single assignment plus a goto.
	//
	// For example code like this:
	//
	//	type T struct{ x int }
	//	func F(x int) *T { return &T{x} }
	//	var Global = F(400)
	//
	// turns into IR like this:
	//
	// 	INLCALL-init
	// 	.   AS2-init
	// 	.   .   DCL # x.go:18:13
	// 	.   .   .   NAME-p.x Class:PAUTO Offset:0 InlFormal OnStack Used int tc(1) # x.go:14:9,x.go:18:13
	// 	.   AS2 Def tc(1) # x.go:18:13
	// 	.   AS2-Lhs
	// 	.   .   NAME-p.x Class:PAUTO Offset:0 InlFormal OnStack Used int tc(1) # x.go:14:9,x.go:18:13
	// 	.   AS2-Rhs
	// 	.   .   LITERAL-400 int tc(1) # x.go:18:14
	// 	.   INLMARK Index:1 # +x.go:18:13
	// 	INLCALL PTR-*T tc(1) # x.go:18:13
	// 	INLCALL-Body
	// 	.   BLOCK tc(1) # x.go:18:13
	// 	.   BLOCK-List
	// 	.   .   DCL tc(1) # x.go:18:13
	// 	.   .   .   NAME-p.~R0 Class:PAUTO Offset:0 OnStack Used PTR-*T tc(1) # x.go:18:13
	// 	.   .   AS2 tc(1) # x.go:18:13
	// 	.   .   AS2-Lhs
	// 	.   .   .   NAME-p.~R0 Class:PAUTO Offset:0 OnStack Used PTR-*T tc(1) # x.go:18:13
	// 	.   .   AS2-Rhs
	// 	.   .   .   INLINED RETURN ARGUMENT HERE
	// 	.   .   GOTO p..i1 tc(1) # x.go:18:13
	// 	.   LABEL p..i1 # x.go:18:13
	// 	INLCALL-ReturnVars
	// 	.   NAME-p.~R0 Class:PAUTO Offset:0 OnStack Used PTR-*T tc(1) # x.go:18:13
	//
	// In non-unified IR, the tree is slightly different:
	//  - if there are no arguments to the inlined function,
	//    the INLCALL-init omits the AS2.
	//  - the DCL inside BLOCK is on the AS2's init list,
	//    not its own statement in the top level of the BLOCK.
	//
	// If the init values are side-effect-free and each either only
	// appears once in the function body or is safely repeatable,
	// then we inline the value expressions into the return argument
	// and then call StaticAssign to handle that copy.
	//
	// This handles simple cases like
	//
	//	var myError = errors.New("mine")
	//
	// where errors.New is
	//
	//	func New(text string) error {
	//		return &errorString{text}
	//	}
	//
	// We could make things more sophisticated but this kind of initializer
	// is the most important case for us to get right.

	init := call.Init()
	var as2init *ir.AssignListStmt
	if len(init) == 2 && init[0].Op() == ir.OAS2 && init[1].Op() == ir.OINLMARK {
		as2init = init[0].(*ir.AssignListStmt)
	} else if len(init) == 1 && init[0].Op() == ir.OINLMARK {
		as2init = new(ir.AssignListStmt)
	} else {
		return false
	}
	if len(call.Body) != 2 || call.Body[0].Op() != ir.OBLOCK || call.Body[1].Op() != ir.OLABEL {
		return false
	}
	label := call.Body[1].(*ir.LabelStmt).Label
	block := call.Body[0].(*ir.BlockStmt)
	list := block.List
	var dcl *ir.Decl
	if len(list) == 3 && list[0].Op() == ir.ODCL {
		dcl = list[0].(*ir.Decl)
		list = list[1:]
	}
	if len(list) != 2 ||
		list[0].Op() != ir.OAS2 ||
		list[1].Op() != ir.OGOTO ||
		list[1].(*ir.BranchStmt).Label != label {
		return false
	}
	as2body := list[0].(*ir.AssignListStmt)
	if dcl == nil {
		ainit := as2body.Init()
		if len(ainit) != 1 || ainit[0].Op() != ir.ODCL {
			return false
		}
		dcl = ainit[0].(*ir.Decl)
	}
	if len(as2body.Lhs) != 1 || as2body.Lhs[0] != dcl.X {
		return false
	}

	// Can't remove the parameter variables if an address is taken.
	for _, v := range as2init.Lhs {
		if v.(*ir.Name).Addrtaken() {
			return false
		}
	}
	// Can't move the computation of the args if they have side effects.
	for _, r := range as2init.Rhs {
		if AnySideEffects(r) {
			return false
		}
	}

	// Can only substitute arg for param if param is used
	// at most once or is repeatable.
	count := make(map[*ir.Name]int)
	for _, x := range as2init.Lhs {
		count[x.(*ir.Name)] = 0
	}
	ir.Visit(as2body.Rhs[0], func(n ir.Node) {
		if name, ok := n.(*ir.Name); ok {
			if c, ok := count[name]; ok {
				count[name] = c + 1
			}
		}
	})
	for name, c := range count {
		if c > 1 {
			// Check whether corresponding initializer can be repeated.
			// Something like 1 can be; make(chan int) or &T{} cannot,
			// because they need to evaluate to the same result in each use.
			for i, n := range as2init.Lhs {
				if n == name && !canRepeat(as2init.Rhs[i]) {
					return false
				}
			}
		}
	}

	// Possible static init.
	// Build tree with args substituted for params and try it.
	args := make(map[*ir.Name]ir.Node)
	for i, v := range as2init.Lhs {
		args[v.(*ir.Name)] = as2init.Rhs[i]
	}
	r, ok := subst(as2body.Rhs[0], args)
	if !ok {
		return false
	}
	ok = s.StaticAssign(l, loff, r, typ)

	if ok && base.Flag.Percent != 0 {
		ir.Dump("static inlined-LEFT", l)
		ir.Dump("static inlined-ORIG", call)
		ir.Dump("static inlined-RIGHT", r)
	}
	return ok
}

// from here down is the walk analysis
// of composite literals.
// most of the work is to generate
// data statements for the constant
// part of the composite literal.

var statuniqgen int // name generator for static temps

// StaticName returns a name backed by a (writable) static data symbol.
// Use readonlystaticname for read-only node.
func StaticName(t *types.Type) *ir.Name {
	// Don't use LookupNum; it interns the resulting string, but these are all unique.
	n := typecheck.NewName(typecheck.Lookup(fmt.Sprintf("%s%d", obj.StaticNamePref, statuniqgen)))
	statuniqgen++
	typecheck.Declare(n, ir.PEXTERN)
	n.SetType(t)
	n.Linksym().Set(obj.AttrStatic, true)
	return n
}

// StaticLoc returns the static address of n, if n has one, or else nil.
func StaticLoc(n ir.Node) (name *ir.Name, offset int64, ok bool) {
	if n == nil {
		return nil, 0, false
	}

	switch n.Op() {
	case ir.ONAME:
		n := n.(*ir.Name)
		return n, 0, true

	case ir.OMETHEXPR:
		n := n.(*ir.SelectorExpr)
		return StaticLoc(n.FuncName())

	case ir.ODOT:
		n := n.(*ir.SelectorExpr)
		if name, offset, ok = StaticLoc(n.X); !ok {
			break
		}
		offset += n.Offset()
		return name, offset, true

	case ir.OINDEX:
		n := n.(*ir.IndexExpr)
		if n.X.Type().IsSlice() {
			break
		}
		if name, offset, ok = StaticLoc(n.X); !ok {
			break
		}
		l := getlit(n.Index)
		if l < 0 {
			break
		}

		// Check for overflow.
		if n.Type().Size() != 0 && types.MaxWidth/n.Type().Size() <= int64(l) {
			break
		}
		offset += int64(l) * n.Type().Size()
		return name, offset, true
	}

	return nil, 0, false
}

func isSideEffect(n ir.Node) bool {
	switch n.Op() {
	// Assume side effects unless we know otherwise.
	default:
		return true

	// No side effects here (arguments are checked separately).
	case ir.ONAME,
		ir.ONONAME,
		ir.OTYPE,
		ir.OLITERAL,
		ir.ONIL,
		ir.OADD,
		ir.OSUB,
		ir.OOR,
		ir.OXOR,
		ir.OADDSTR,
		ir.OADDR,
		ir.OANDAND,
		ir.OBYTES2STR,
		ir.ORUNES2STR,
		ir.OSTR2BYTES,
		ir.OSTR2RUNES,
		ir.OCAP,
		ir.OCOMPLIT,
		ir.OMAPLIT,
		ir.OSTRUCTLIT,
		ir.OARRAYLIT,
		ir.OSLICELIT,
		ir.OPTRLIT,
		ir.OCONV,
		ir.OCONVIFACE,
		ir.OCONVNOP,
		ir.ODOT,
		ir.OEQ,
		ir.ONE,
		ir.OLT,
		ir.OLE,
		ir.OGT,
		ir.OGE,
		ir.OKEY,
		ir.OSTRUCTKEY,
		ir.OLEN,
		ir.OMUL,
		ir.OLSH,
		ir.ORSH,
		ir.OAND,
		ir.OANDNOT,
		ir.ONEW,
		ir.ONOT,
		ir.OBITNOT,
		ir.OPLUS,
		ir.ONEG,
		ir.OOROR,
		ir.OPAREN,
		ir.ORUNESTR,
		ir.OREAL,
		ir.OIMAG,
		ir.OCOMPLEX:
		return false

	// Only possible side effect is division by zero.
	case ir.ODIV, ir.OMOD:
		n := n.(*ir.BinaryExpr)
		if n.Y.Op() != ir.OLITERAL || constant.Sign(n.Y.Val()) == 0 {
			return true
		}

	// Only possible side effect is panic on invalid size,
	// but many makechan and makemap use size zero, which is definitely OK.
	case ir.OMAKECHAN, ir.OMAKEMAP:
		n := n.(*ir.MakeExpr)
		if !ir.IsConst(n.Len, constant.Int) || constant.Sign(n.Len.Val()) != 0 {
			return true
		}

	// Only possible side effect is panic on invalid size.
	// TODO(rsc): Merge with previous case (probably breaks toolstash -cmp).
	case ir.OMAKESLICE, ir.OMAKESLICECOPY:
		return true
	}
	return false
}

// AnySideEffects reports whether n contains any operations that could have observable side effects.
func AnySideEffects(n ir.Node) bool {
	return ir.Any(n, isSideEffect)
}

// canRepeat reports whether executing n multiple times has the same effect as
// assigning n to a single variable and using that variable multiple times.
func canRepeat(n ir.Node) bool {
	bad := func(n ir.Node) bool {
		if isSideEffect(n) {
			return true
		}
		switch n.Op() {
		case ir.OMAKECHAN,
			ir.OMAKEMAP,
			ir.OMAKESLICE,
			ir.OMAKESLICECOPY,
			ir.OMAPLIT,
			ir.ONEW,
			ir.OPTRLIT,
			ir.OSLICELIT,
			ir.OSTR2BYTES,
			ir.OSTR2RUNES:
			return true
		}
		return false
	}
	return !ir.Any(n, bad)
}

func getlit(lit ir.Node) int {
	if ir.IsSmallIntConst(lit) {
		return int(ir.Int64Val(lit))
	}
	return -1
}

func isvaluelit(n ir.Node) bool {
	return n.Op() == ir.OARRAYLIT || n.Op() == ir.OSTRUCTLIT
}

func subst(n ir.Node, m map[*ir.Name]ir.Node) (ir.Node, bool) {
	valid := true
	var edit func(ir.Node) ir.Node
	edit = func(x ir.Node) ir.Node {
		switch x.Op() {
		case ir.ONAME:
			x := x.(*ir.Name)
			if v, ok := m[x]; ok {
				return ir.DeepCopy(v.Pos(), v)
			}
			return x
		case ir.ONONAME, ir.OLITERAL, ir.ONIL, ir.OTYPE:
			return x
		}
		x = ir.Copy(x)
		ir.EditChildren(x, edit)
		if x, ok := x.(*ir.ConvExpr); ok && x.X.Op() == ir.OLITERAL {
			// A conversion of variable or expression involving variables
			// may become a conversion of constant after inlining the parameters
			// and doing constant evaluation. Truncations that were valid
			// on variables are not valid on constants, so we might have
			// generated invalid code that will trip up the rest of the compiler.
			// Fix those by truncating the constants.
			if x, ok := truncate(x.X.(*ir.ConstExpr), x.Type()); ok {
				return x
			}
			valid = false
			return x
		}
		return typecheck.EvalConst(x)
	}
	n = edit(n)
	return n, valid
}

// truncate returns the result of force converting c to type t,
// truncating its value as needed, like a conversion of a variable.
// If the conversion is too difficult, truncate returns nil, false.
func truncate(c *ir.ConstExpr, t *types.Type) (*ir.ConstExpr, bool) {
	ct := c.Type()
	cv := c.Val()
	if ct.Kind() != t.Kind() {
		switch {
		default:
			// Note: float -> float/integer and complex -> complex are valid but subtle.
			// For example a float32(float64 1e300) evaluates to +Inf at runtime
			// and the compiler doesn't have any concept of +Inf, so that would
			// have to be left for runtime code evaluation.
			// For now
			return nil, false

		case ct.IsInteger() && t.IsInteger():
			// truncate or sign extend
			bits := t.Size() * 8
			cv = constant.BinaryOp(cv, token.AND, constant.MakeUint64(1<<bits-1))
			if t.IsSigned() && constant.Compare(cv, token.GEQ, constant.MakeUint64(1<<(bits-1))) {
				cv = constant.BinaryOp(cv, token.OR, constant.MakeInt64(-1<<(bits-1)))
			}
		}
	}
	c = ir.NewConstExpr(cv, c).(*ir.ConstExpr)
	c.SetType(t)
	return c, true
}
