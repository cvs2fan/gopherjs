package compiler

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"

	"github.com/gopherjs/gopherjs/compiler/analysis"
	"github.com/gopherjs/gopherjs/compiler/typesutil"

	"golang.org/x/tools/go/types"
)

func (c *funcContext) Write(b []byte) (int, error) {
	c.writePos()
	c.output = append(c.output, b...)
	return len(b), nil
}

func (c *funcContext) Printf(format string, values ...interface{}) {
	c.Write([]byte(strings.Repeat("\t", c.p.indentation)))
	fmt.Fprintf(c, format, values...)
	c.Write([]byte{'\n'})
	c.Write(c.delayedOutput)
	c.delayedOutput = nil
}

func (c *funcContext) PrintCond(cond bool, onTrue, onFalse string) {
	if !cond {
		c.Printf("/* %s */ %s", strings.Replace(onTrue, "*/", "<star>/", -1), onFalse)
		return
	}
	c.Printf("%s", onTrue)
}

func (c *funcContext) SetPos(pos token.Pos) {
	c.posAvailable = true
	c.pos = pos
}

func (c *funcContext) writePos() {
	if c.posAvailable {
		c.posAvailable = false
		c.Write([]byte{'\b'})
		binary.Write(c, binary.BigEndian, uint32(c.pos))
	}
}

func (c *funcContext) Indent(f func()) {
	c.p.indentation++
	f()
	c.p.indentation--
}

func (c *funcContext) CatchOutput(indent int, f func()) []byte {
	origoutput := c.output
	c.output = nil
	c.p.indentation += indent
	f()
	c.writePos()
	catched := c.output
	c.output = origoutput
	c.p.indentation -= indent
	return catched
}

func (c *funcContext) Delayed(f func()) {
	c.delayedOutput = c.CatchOutput(0, f)
}

func (c *funcContext) translateArgs(sig *types.Signature, argExprs []ast.Expr, ellipsis, clone bool) []string {
	if len(argExprs) == 1 {
		if tuple, isTuple := c.p.Types[argExprs[0]].Type.(*types.Tuple); isTuple {
			tupleVar := c.newVariable("_tuple")
			c.Printf("%s = %s;", tupleVar, c.translateExpr(argExprs[0]))
			argExprs = make([]ast.Expr, tuple.Len())
			for i := range argExprs {
				argExprs[i] = c.newIdent(c.formatExpr("%s[%d]", tupleVar, i).String(), tuple.At(i).Type())
			}
		}
	}

	paramsLen := sig.Params().Len()

	var varargType *types.Slice
	if sig.Variadic() && !ellipsis {
		varargType = sig.Params().At(paramsLen - 1).Type().(*types.Slice)
	}

	preserveOrder := false
	for i := 1; i < len(argExprs); i++ {
		preserveOrder = preserveOrder || c.Blocking[argExprs[i]]
	}

	args := make([]string, len(argExprs))
	for i, argExpr := range argExprs {
		var argType types.Type
		switch {
		case varargType != nil && i >= paramsLen-1:
			argType = varargType.Elem()
		default:
			argType = sig.Params().At(i).Type()
		}

		var arg string
		switch {
		case clone:
			arg = c.translateImplicitConversionWithCloning(argExpr, argType).String()
		default:
			arg = c.translateImplicitConversion(argExpr, argType).String()
		}

		if preserveOrder && c.p.Types[argExpr].Value == nil {
			argVar := c.newVariable("_arg")
			c.Printf("%s = %s;", argVar, arg)
			arg = argVar
		}

		args[i] = arg
	}

	if varargType != nil {
		return append(args[:paramsLen-1], fmt.Sprintf("new %s([%s])", c.typeName(varargType), strings.Join(args[paramsLen-1:], ", ")))
	}
	return args
}

func (c *funcContext) translateSelection(sel *types.Selection, pos token.Pos) ([]string, string) {
	var fields []string
	t := sel.Recv()
	for _, index := range sel.Index() {
		if ptr, isPtr := t.(*types.Pointer); isPtr {
			t = ptr.Elem()
		}
		s := t.Underlying().(*types.Struct)
		if jsTag := getJsTag(s.Tag(index)); jsTag != "" {
			jsFieldName := s.Field(index).Name()
			for {
				fields = append(fields, fieldName(s, 0))
				ft := s.Field(0).Type()
				if typesutil.IsJsObject(ft) {
					return fields, jsTag
				}
				ft = ft.Underlying()
				if ptr, ok := ft.(*types.Pointer); ok {
					ft = ptr.Elem().Underlying()
				}
				var ok bool
				s, ok = ft.(*types.Struct)
				if !ok || s.NumFields() == 0 {
					c.p.errList = append(c.p.errList, types.Error{Fset: c.p.fileSet, Pos: pos, Msg: fmt.Sprintf("could not find field with type *js.Object for 'js' tag of field '%s'", jsFieldName), Soft: true})
					return nil, ""
				}
			}
		}
		fields = append(fields, fieldName(s, index))
		t = s.Field(index).Type()
	}
	return fields, ""
}

func (c *funcContext) zeroValue(ty types.Type) string {
	if typesutil.IsJsObject(ty) {
		return "null"
	}
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		switch {
		case is64Bit(t) || isComplex(t):
			return fmt.Sprintf("new %s(0, 0)", c.typeName(ty))
		case isBoolean(t):
			return "false"
		case isNumeric(t), t.Kind() == types.UnsafePointer:
			return "0"
		case isString(t):
			return `""`
		case t.Kind() == types.UntypedNil:
			panic("Zero value for untyped nil.")
		default:
			panic("Unhandled type")
		}
	case *types.Array:
		return fmt.Sprintf("%s.zero()", c.typeName(ty))
	case *types.Signature:
		return "$throwNilPointerError"
	case *types.Slice:
		return fmt.Sprintf("%s.nil", c.typeName(ty))
	case *types.Struct:
		return fmt.Sprintf("new %s.ptr()", c.typeName(ty))
	case *types.Map:
		return "false"
	case *types.Interface:
		return "$ifaceNil"
	}
	return fmt.Sprintf("%s.nil", c.typeName(ty))
}

func (c *funcContext) newVariable(name string) string {
	return c.newVariableWithLevel(name, false)
}

func (c *funcContext) newVariableWithLevel(name string, pkgLevel bool) string {
	if name == "" {
		panic("newVariable: empty name")
	}
	for _, b := range []byte(name) {
		if b < '0' || b > 'z' {
			name = "nonAsciiName"
			break
		}
	}
	if c.p.minify {
		i := 0
		for {
			offset := int('a')
			if pkgLevel {
				offset = int('A')
			}
			j := i
			name = ""
			for {
				name = string(offset+(j%26)) + name
				j = j/26 - 1
				if j == -1 {
					break
				}
			}
			if c.allVars[name] == 0 {
				break
			}
			i++
		}
	}
	n := c.allVars[name]
	c.allVars[name] = n + 1
	varName := name
	if n > 0 {
		varName = fmt.Sprintf("%s$%d", name, n)
	}

	if pkgLevel {
		for c2 := c.parent; c2 != nil; c2 = c2.parent {
			c2.allVars[name] = n + 1
		}
		return varName
	}

	c.localVars = append(c.localVars, varName)
	return varName
}

func (c *funcContext) newIdent(name string, t types.Type) *ast.Ident {
	ident := ast.NewIdent(name)
	c.setType(ident, t)
	obj := types.NewVar(0, c.p.Pkg, name, t)
	c.p.Uses[ident] = obj
	c.p.objectNames[obj] = name
	return ident
}

func (c *funcContext) setType(e ast.Expr, t types.Type) ast.Expr {
	c.p.Types[e] = types.TypeAndValue{Type: t}
	return e
}

func (c *funcContext) objectName(o types.Object) string {
	if o.Pkg() != c.p.Pkg || o.Parent() == c.p.Pkg.Scope() {
		c.p.dependencies[o] = true
	}

	if o.Pkg() != c.p.Pkg {
		pkgVar, found := c.p.pkgVars[o.Pkg().Path()]
		if !found {
			pkgVar = fmt.Sprintf(`$packages["%s"]`, o.Pkg().Path())
		}
		return pkgVar + "." + o.Name()
	}

	switch o.(type) {
	case *types.Var, *types.Const:
		if o.Exported() && o.Parent() == c.p.Pkg.Scope() {
			return "$pkg." + o.Name()
		}
	}

	name, ok := c.p.objectNames[o]
	if !ok {
		name = c.newVariableWithLevel(o.Name(), o.Parent() == c.p.Pkg.Scope())
		c.p.objectNames[o] = name
	}

	if v, ok := o.(*types.Var); ok && c.p.escapingVars[v] {
		return name + "[0]"
	}
	return name
}

func (c *funcContext) varPtrName(v *types.Var) string {
	name, ok := c.p.varPtrNames[v]
	if !ok {
		name = c.newVariableWithLevel(v.Name()+"_ptr", v.Parent() == c.p.Pkg.Scope())
		c.p.varPtrNames[v] = name
	}
	return name
}

func (c *funcContext) typeName(ty types.Type) string {
	switch t := ty.(type) {
	case *types.Basic:
		return "$" + toJavaScriptType(t)
	case *types.Named:
		if t.Obj().Name() == "error" {
			return "$error"
		}
		return c.objectName(t.Obj())
	case *types.Interface:
		if t.Empty() {
			return "$emptyInterface"
		}
	}

	anonType, ok := c.p.anonTypeMap.At(ty).(*types.TypeName)
	if !ok {
		c.initArgs(ty) // cause all embedded types to be registered
		varName := c.newVariableWithLevel(strings.ToLower(typeKind(ty)[5:])+"Type", true)
		anonType = types.NewTypeName(token.NoPos, c.p.Pkg, varName, ty) // fake types.TypeName
		c.p.anonTypes = append(c.p.anonTypes, anonType)
		c.p.anonTypeMap.Set(ty, anonType)
	}
	c.p.dependencies[anonType] = true
	return anonType.Name()
}

func (c *funcContext) makeKey(expr ast.Expr, keyType types.Type) string {
	switch t := keyType.Underlying().(type) {
	case *types.Array, *types.Struct:
		return fmt.Sprintf("(new %s(%s)).$key()", c.typeName(keyType), c.translateExpr(expr))
	case *types.Basic:
		if is64Bit(t) || isComplex(t) {
			return fmt.Sprintf("%s.$key()", c.translateExpr(expr))
		}
		if isFloat(t) {
			return fmt.Sprintf("$floatKey(%s)", c.translateExpr(expr))
		}
		return c.translateImplicitConversion(expr, keyType).String()
	case *types.Chan, *types.Pointer, *types.Interface:
		return fmt.Sprintf("%s.$key()", c.translateImplicitConversion(expr, keyType))
	default:
		return c.translateImplicitConversion(expr, keyType).String()
	}
}

func (c *funcContext) externalize(s string, t types.Type) string {
	if typesutil.IsJsObject(t) {
		return s
	}
	switch u := t.Underlying().(type) {
	case *types.Basic:
		if isNumeric(u) && !is64Bit(u) && !isComplex(u) {
			return s
		}
		if u.Kind() == types.UntypedNil {
			return "null"
		}
	}
	return fmt.Sprintf("$externalize(%s, %s)", s, c.typeName(t))
}

func (c *funcContext) handleEscapingVars(n ast.Node) {
	newEscapingVars := make(map[*types.Var]bool)
	for escaping := range c.p.escapingVars {
		newEscapingVars[escaping] = true
	}
	c.p.escapingVars = newEscapingVars

	var names []string
	for obj := range analysis.EscapingObjects(n, c.p.Info.Info) {
		names = append(names, c.objectName(obj))
		c.p.escapingVars[obj] = true
	}
	sort.Strings(names)
	for _, name := range names {
		c.Printf("%s = [%s];", name, name)
	}
}

func fieldName(t *types.Struct, i int) string {
	name := t.Field(i).Name()
	if name == "_" || reservedKeywords[name] {
		return fmt.Sprintf("%s$%d", name, i)
	}
	return name
}

func typeKind(ty types.Type) string {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		return "$kind" + toJavaScriptType(t)
	case *types.Array:
		return "$kindArray"
	case *types.Chan:
		return "$kindChan"
	case *types.Interface:
		return "$kindInterface"
	case *types.Map:
		return "$kindMap"
	case *types.Signature:
		return "$kindFunc"
	case *types.Slice:
		return "$kindSlice"
	case *types.Struct:
		return "$kindStruct"
	case *types.Pointer:
		return "$kindPtr"
	default:
		panic(fmt.Sprintf("Unhandled type: %T\n", t))
	}
}

func toJavaScriptType(t *types.Basic) string {
	switch t.Kind() {
	case types.UntypedInt:
		return "Int"
	case types.Byte:
		return "Uint8"
	case types.Rune:
		return "Int32"
	case types.UnsafePointer:
		return "UnsafePointer"
	default:
		name := t.String()
		return strings.ToUpper(name[:1]) + name[1:]
	}
}

func is64Bit(t *types.Basic) bool {
	return t.Kind() == types.Int64 || t.Kind() == types.Uint64
}

func isBoolean(t *types.Basic) bool {
	return t.Info()&types.IsBoolean != 0
}

func isComplex(t *types.Basic) bool {
	return t.Info()&types.IsComplex != 0
}

func isFloat(t *types.Basic) bool {
	return t.Info()&types.IsFloat != 0
}

func isInteger(t *types.Basic) bool {
	return t.Info()&types.IsInteger != 0
}

func isNumeric(t *types.Basic) bool {
	return t.Info()&types.IsNumeric != 0
}

func isString(t *types.Basic) bool {
	return t.Info()&types.IsString != 0
}

func isUnsigned(t *types.Basic) bool {
	return t.Info()&types.IsUnsigned != 0
}

func isBlank(expr ast.Expr) bool {
	if expr == nil {
		return true
	}
	if id, isIdent := expr.(*ast.Ident); isIdent {
		return id.Name == "_"
	}
	return false
}

func isWrapped(ty types.Type) bool {
	switch t := ty.Underlying().(type) {
	case *types.Basic:
		return !is64Bit(t) && !isComplex(t) && t.Kind() != types.UntypedNil
	case *types.Array, *types.Map, *types.Signature:
		return true
	case *types.Pointer:
		_, isArray := t.Elem().Underlying().(*types.Array)
		return isArray
	}
	return false
}

func encodeString(s string) string {
	buffer := bytes.NewBuffer(nil)
	for _, r := range []byte(s) {
		switch r {
		case '\b':
			buffer.WriteString(`\b`)
		case '\f':
			buffer.WriteString(`\f`)
		case '\n':
			buffer.WriteString(`\n`)
		case '\r':
			buffer.WriteString(`\r`)
		case '\t':
			buffer.WriteString(`\t`)
		case '\v':
			buffer.WriteString(`\v`)
		case '"':
			buffer.WriteString(`\"`)
		case '\\':
			buffer.WriteString(`\\`)
		default:
			if r < 0x20 || r > 0x7E {
				fmt.Fprintf(buffer, `\x%02X`, r)
				continue
			}
			buffer.WriteByte(r)
		}
	}
	return `"` + buffer.String() + `"`
}

func getJsTag(tag string) string {
	for tag != "" {
		// skip leading space
		i := 0
		for i < len(tag) && tag[i] == ' ' {
			i++
		}
		tag = tag[i:]
		if tag == "" {
			break
		}

		// scan to colon.
		// a space or a quote is a syntax error
		i = 0
		for i < len(tag) && tag[i] != ' ' && tag[i] != ':' && tag[i] != '"' {
			i++
		}
		if i+1 >= len(tag) || tag[i] != ':' || tag[i+1] != '"' {
			break
		}
		name := string(tag[:i])
		tag = tag[i+1:]

		// scan quoted string to find value
		i = 1
		for i < len(tag) && tag[i] != '"' {
			if tag[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(tag) {
			break
		}
		qvalue := string(tag[:i+1])
		tag = tag[i+1:]

		if name == "js" {
			value, _ := strconv.Unquote(qvalue)
			return value
		}
	}
	return ""
}

func needsSpace(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '$'
}

func removeWhitespace(b []byte, minify bool) []byte {
	if !minify {
		return b
	}

	var out []byte
	var previous byte
	for len(b) > 0 {
		switch b[0] {
		case '\b':
			out = append(out, b[:5]...)
			b = b[5:]
			continue
		case ' ', '\t', '\n':
			if (!needsSpace(previous) || !needsSpace(b[1])) && !(previous == '-' && b[1] == '-') {
				b = b[1:]
				continue
			}
		case '"':
			out = append(out, '"')
			b = b[1:]
			for {
				i := bytes.IndexAny(b, "\"\\")
				out = append(out, b[:i]...)
				b = b[i:]
				if b[0] == '"' {
					break
				}
				// backslash
				out = append(out, b[:2]...)
				b = b[2:]
			}
		case '/':
			if b[1] == '*' {
				i := bytes.Index(b[2:], []byte("*/"))
				b = b[i+4:]
				continue
			}
		}
		out = append(out, b[0])
		previous = b[0]
		b = b[1:]
	}
	return out
}

func rangeCheck(pattern string, constantIndex, array bool) string {
	if constantIndex && array {
		return pattern
	}
	lengthProp := "$length"
	if array {
		lengthProp = "length"
	}
	check := "%2f >= %1e." + lengthProp
	if !constantIndex {
		check = "(%2f < 0 || " + check + ")"
	}
	return "(" + check + ` ? $throwRuntimeError("index out of range") : ` + pattern + ")"
}
