package minilustre

import (
	"fmt"

	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/enum"
	"github.com/llir/llvm/ir/types"
	"github.com/llir/llvm/ir/value"
)

type compiler struct {
	m *ir.Module
	funcs map[string]*ir.Func
}

type context struct {
	b *ir.Block
	f *ir.Func
	vars map[string]value.Value
	glob int
}

func (c *compiler) typ(t Type) types.Type {
	switch t {
	case TypeUnit:
		return types.Void
	case TypeBool:
		return types.I1
	case TypeInt:
		return types.I32
	case TypeFloat:
		return types.Float
	case TypeString:
		return types.I8Ptr
	}
	panic(fmt.Sprintf("unknown type %v", t))
}

func (ctx *context) freshGlobal() string {
	ctx.glob++
	return fmt.Sprintf("_%v_%v", ctx.f.GlobalName, ctx.glob)
}

func (c *compiler) expr(e Expr, ctx *context) (value.Value, error) {
	switch e := e.(type) {
	case *ExprCall:
		f, ok := c.funcs[e.Name]
		if !ok {
			return nil, fmt.Errorf("minilustre: undefined node '%v'", e.Name)
		}
		args := make([]value.Value, len(e.Args))
		for i, arg := range e.Args {
			var err error
			args[i], err = c.expr(arg, ctx)
			if err != nil {
				return nil, err
			}
		}
		return ctx.b.NewCall(f, args...), nil
	case ExprConst:
		switch v := e.Value.(type) {
		case bool:
			var i int64 = 0
			if v {
				i = 1
			}
			return constant.NewInt(types.I1, i), nil
		case int:
			return constant.NewInt(types.I32, int64(v)), nil
		case string:
			b := append([]byte(v), 0)
			glob := c.m.NewGlobalDef(ctx.freshGlobal(), constant.NewCharArray(b))
			glob.Immutable = true
			glob.Linkage = enum.LinkagePrivate
			zero := constant.NewInt(types.I64, 0)
			ptr := ctx.b.NewGetElementPtr(glob, zero, zero)
			return ptr, nil
		default:
			panic(fmt.Sprintf("unknown const type %T", v))
		}
	case ExprVar:
		v, ok := ctx.vars[string(e)]
		if !ok {
			//panic(fmt.Sprintf("referring to undefined variable '%v'", string(e)))
			return nil, fmt.Errorf("minilustre: referring to unknown variable '%v'", string(e))
		}
		// if _, ok := v.(*constant.Undef); ok {
		// 	return nil, fmt.Errorf("minilustre: referring to undefined variable '%v'", string(e))
		// }
		return v, nil
	case ExprTuple:
		values := make([]value.Value, len(e))
		typs := make([]types.Type, len(e))
		for i, ee := range e {
			var err error
			values[i], err = c.expr(ee, ctx)
			if err != nil {
				return nil, err
			}
			typs[i] = values[i].Type()
		}

		// TODO: maybe don't use globals for tuples
		glob := c.m.NewGlobalDef(ctx.freshGlobal(), constant.NewUndef(types.NewStruct(typs...)))
		glob.Linkage = enum.LinkagePrivate

		for i, v := range values {
			ptr := ctx.b.NewGetElementPtr(glob, constant.NewInt(types.I32, 0), constant.NewInt(types.I32, int64(i)))
			ptr.InBounds = true
			ctx.b.NewStore(v, ptr)
		}

		return glob, nil
	case *ExprBinOp:
		left, err := c.expr(e.Left, ctx)
		if err != nil {
			return nil, err
		}

		right, err := c.expr(e.Right, ctx)
		if err != nil {
			return nil, err
		}

		switch e.Op {
		case BinOpMinus:
			return ctx.b.NewSub(left, right), nil
		case BinOpPlus:
			return ctx.b.NewAdd(left, right), nil
		case BinOpGt:
			return ctx.b.NewICmp(enum.IPredSGT, left, right), nil
		case BinOpLt:
			return ctx.b.NewICmp(enum.IPredSLT, left, right), nil
		case BinOpFby:
			return left, nil // TODO
		}
		panic(fmt.Sprintf("unknown binary operation %v", e.Op))
	case *ExprIf:
		cond, err := c.expr(e.Cond, ctx)
		if err != nil {
			return nil, err
		}

		body, err := c.expr(e.Body, ctx)
		if err != nil {
			return nil, err
		}

		els, err := c.expr(e.Else, ctx)
		if err != nil {
			return nil, err
		}

		return ctx.b.NewSelect(cond, body, els), nil
	default:
		panic(fmt.Sprintf("unknown expression %T", e))
	}
}

func (ctx *context) setVar(name string, v value.Value) error {
	if v, ok := ctx.vars[name]; ok {
		if _, ok := v.(*constant.Undef); !ok {
			return fmt.Errorf("minilustre: cannot write variable '%v' twice", name)
		}
	}

	ctx.vars[name] = v
	return nil
}

func (c *compiler) assign(assign *Assign, ctx *context) error {
	v, err := c.expr(assign.Body, ctx)
	if err != nil {
		return err
	}

	if len(assign.Dst) == 1 {
		return ctx.setVar(assign.Dst[0], v)
	} else if len(assign.Dst) > 1 {
		for i, dst := range assign.Dst {
			ptr := ctx.b.NewGetElementPtr(v, constant.NewInt(types.I32, 0), constant.NewInt(types.I32, int64(i)))
			ptr.InBounds = true
			vv := ctx.b.NewLoad(ptr)
			if err := ctx.setVar(dst, vv); err != nil {
				return err
			}
		}

		return nil
	} else {
		panic("cannot assign to nothing")
	}
}

func (c *compiler) node(n *Node) error {
	vars := make(map[string]value.Value, len(n.InParams) + len(n.OutParams) + len(n.LocalParams))
	params := make([]*ir.Param, 0, len(n.InParams))
	retTypes := make([]types.Type, 0, len(n.OutParams))
	for name, typ := range n.InParams {
		if typ != TypeUnit {
			p := ir.NewParam(name, c.typ(typ))
			params = append(params, p)
			vars[name] = p
		}
	}
	for name, typ := range n.OutParams {
		// TODO
		vars[name] = constant.NewUndef(c.typ(typ))
		retTypes = append(retTypes, vars[name].Type())
	}
	for name, typ := range n.LocalParams {
		vars[name] = constant.NewUndef(c.typ(typ))
	}

	var retType types.Type = types.Void
	if len(retTypes) == 1 {
		retType = retTypes[0]
	} else {
		retType = types.NewPointer(types.NewStruct(retTypes...))
	}

	f := c.m.NewFunc(n.Name, retType, params...)
	entry := f.NewBlock("")

	ctx := context{b: entry, f: f, vars: vars}
	for _, assign := range n.Body {
		if err := c.assign(&assign, &ctx); err != nil {
			return fmt.Errorf("failed to compile node '%v': %v", n.Name, err)
		}
	}

	var ret value.Value
	if len(n.OutParams) == 1 {
		var name string
		for name = range n.OutParams {}
		ret = vars[name]
	} else {
		glob := c.m.NewGlobalDef(ctx.freshGlobal(), constant.NewUndef(types.NewStruct(retTypes...)))
		glob.Linkage = enum.LinkagePrivate

		i := 0
		for name := range n.OutParams {
			ptr := ctx.b.NewGetElementPtr(glob, constant.NewInt(types.I32, 0), constant.NewInt(types.I32, int64(i)))
			ptr.InBounds = true
			ctx.b.NewStore(vars[name], ptr)
			i++
		}

		ret = glob
	}

	entry.NewRet(ret)

	c.funcs[n.Name] = f
	return nil
}

func Compile(f *File, m *ir.Module) error {
	c := compiler{
		m: m,
		funcs: map[string]*ir.Func{
			"print": m.NewFunc("print", types.Void, ir.NewParam("str", types.I8Ptr)),
		},
	}

	for _, n := range f.Nodes {
		if err := c.node(&n); err != nil {
			return err
		}
	}

	return nil
}
