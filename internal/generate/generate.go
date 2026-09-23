// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

// Package generate turns the Tailscale SDK's resource methods into Cobra commands.
package generate

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"unicode"
)

type parameter struct {
	name, typ, kind string
	required        bool
	shape           *shape
	expr            ast.Expr
	options         []parameter
	constructor     string
	handlerType     string
}

type operation struct {
	name, doc    string
	params       []parameter
	result       bool
	output       ast.Expr
	outputShape  *shape
	outputFormat string
}

type resource struct {
	name, doc string
	ops       []operation
}

type index struct {
	types     map[string]ast.Expr
	methods   map[string][]*ast.FuncDecl
	enums     map[string][]string
	functions []*ast.FuncDecl
}

// Generate reads non-test Go files in dir and emits a complete command tree.
// Unsupported signatures are errors, never silently omitted.
func Generate(dir string) ([]byte, error) {
	idx := index{types: map[string]ast.Expr{}, methods: map[string][]*ast.FuncDecl{}, enums: map[string][]string{}}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, entry.Name()), nil, parser.ParseComments)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok == token.TYPE {
					for _, spec := range d.Specs {
						if t, ok := spec.(*ast.TypeSpec); ok {
							idx.types[t.Name.Name] = t.Type
						}
					}
				}
				if d.Tok == token.CONST {
					var previousType ast.Expr
					for _, spec := range d.Specs {
						v := spec.(*ast.ValueSpec)
						typ := v.Type
						if len(v.Values) == 0 {
							typ = previousType
						}
						previousType = typ
						if name, ok := typ.(*ast.Ident); ok && closedStringEnums[name.Name] {
							for _, n := range v.Names {
								if n.IsExported() {
									idx.enums[name.Name] = append(idx.enums[name.Name], n.Name)
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil && d.Name.IsExported() {
					idx.functions = append(idx.functions, d)
				}
				if d.Recv == nil || !d.Name.IsExported() {
					continue
				}
				recv := strings.TrimPrefix(typeString(d.Recv.List[0].Type), "*")
				idx.methods[recv] = append(idx.methods[recv], d)
			}
		}
	}
	var resources []resource
	for _, method := range idx.methods["Client"] {
		if deprecated(method) {
			continue
		}
		if method.Type.Params.NumFields() != 0 || method.Type.Results.NumFields() != 1 {
			return nil, fmt.Errorf("Client.%s: expected resource accessor", method.Name)
		}
		typ := typeString(method.Type.Results.List[0].Type)
		if !strings.HasPrefix(typ, "*") || !strings.HasSuffix(typ, "Resource") {
			return nil, fmt.Errorf("Client.%s: unexpected return %s", method.Name, typ)
		}
		r := resource{name: method.Name.Name, doc: method.Doc.Text()}
		methods := idx.methods[strings.TrimPrefix(typ, "*")]
		if len(methods) == 0 {
			return nil, fmt.Errorf("%s: no resource methods", r.name)
		}
		for _, m := range methods {
			if deprecated(m) {
				continue
			}
			op, err := idx.operation(r.name, m)
			if err != nil {
				return nil, err
			}
			r.ops = append(r.ops, op)
		}
		slices.SortFunc(r.ops, func(a, b operation) int { return strings.Compare(a.name, b.name) })
		resources = append(resources, r)
	}
	if len(resources) == 0 {
		return nil, fmt.Errorf("no Client resource accessors found in %s", dir)
	}
	slices.SortFunc(resources, func(a, b resource) int { return strings.Compare(a.name, b.name) })
	var enums []enumData
	for name := range closedStringEnums {
		if _, exists := idx.types[name]; !exists {
			continue
		}
		kind, err := idx.kind(ast.NewIdent(name), map[string]bool{})
		if err != nil || kind != "string" || len(idx.enums[name]) == 0 {
			return nil, fmt.Errorf("enum %s: expected string type with exported typed constants", name)
		}
		slices.Sort(idx.enums[name])
		enums = append(enums, enumData{Type: name, Constants: idx.enums[name]})
	}
	slices.SortFunc(enums, func(a, b enumData) int { return strings.Compare(a.Type, b.Type) })
	inputs := newShapeBuilder(idx, decode)
	outputs := newShapeBuilder(idx, encode)
	for ri := range resources {
		for oi := range resources[ri].ops {
			op := &resources[ri].ops[oi]
			if op.output != nil {
				var err error
				op.outputShape, err = outputs.shape(op.output)
				if err != nil {
					return nil, fmt.Errorf("%s.%s output: %w", resources[ri].name, op.name, err)
				}
			}
			for pi := range op.params {
				p := &op.params[pi]
				if err := buildParameterShapes(inputs, p); err != nil {
					return nil, fmt.Errorf("%s.%s %w", resources[ri].name, op.name, err)
				}
			}
		}
	}
	return emit(resources, enums, inputs.defs, outputs.defs)
}

func deprecated(f *ast.FuncDecl) bool { return strings.Contains(f.Doc.Text(), "Deprecated:") }

func typeString(e ast.Expr) string { return types.ExprString(e) }

func (idx index) kind(e ast.Expr, seen map[string]bool) (string, error) {
	switch t := e.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string", "bool":
			return t.Name, nil
		}
		if seen[t.Name] {
			return "", fmt.Errorf("cyclic or unsupported type %s", t.Name)
		}
		seen[t.Name] = true
		if underlying, ok := idx.types[t.Name]; ok {
			return idx.kind(underlying, seen)
		}
	case *ast.StarExpr:
		return idx.kind(t.X, seen)
	case *ast.ArrayType:
		if t.Len == nil {
			k, err := idx.kind(t.Elt, seen)
			if err == nil && k == "string" {
				return "strings", nil
			}
		}
		return "json", nil
	case *ast.StructType, *ast.MapType:
		return "json", nil
	}
	return "", fmt.Errorf("unsupported input type %s", typeString(e))
}

func qualified(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		switch t.Name {
		case "string", "bool", "any", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune", "float32", "float64":
			return t.Name
		}
		return "tailscale." + t.Name
	case *ast.StarExpr:
		return "*" + qualified(t.X)
	case *ast.ArrayType:
		if t.Len == nil {
			return "[]" + qualified(t.Elt)
		}
		return "[" + typeString(t.Len) + "]" + qualified(t.Elt)
	case *ast.MapType:
		return "map[" + qualified(t.Key) + "]" + qualified(t.Value)
	}
	return typeString(e)
}

func (idx index) operation(resource string, m *ast.FuncDecl) (operation, error) {
	op := operation{name: m.Name.Name, doc: m.Doc.Text()}
	fail := func(err error) (operation, error) { return op, fmt.Errorf("%s.%s: %w", resource, m.Name, err) }
	fields := m.Type.Params.List
	if len(fields) == 0 || typeString(fields[0].Type) != "context.Context" || len(fields[0].Names) != 1 {
		return fail(fmt.Errorf("expected context as first parameter"))
	}
	results := m.Type.Results
	if results.NumFields() < 1 || results.NumFields() > 2 || typeString(results.List[len(results.List)-1].Type) != "error" {
		return fail(fmt.Errorf("expected error or (T, error)"))
	}
	op.result = results.NumFields() == 2
	if op.result {
		op.output = results.List[0].Type
		op.outputFormat = "JSON"
	}
	for _, f := range fields[1:] {
		for _, n := range f.Names {
			p := parameter{name: n.Name, typ: qualified(f.Type), required: true, expr: f.Type}
			typ := typeString(f.Type)
			switch {
			case isVariadic(f.Type):
				var err error
				p, err = idx.optionParameter(p)
				if err != nil {
					return fail(err)
				}
			case idx.callbackValue(f.Type) != nil:
				if op.result || op.output != nil {
					return fail(fmt.Errorf("handler requires a single callback and error-only result"))
				}
				p.kind = "handler"
				op.output = idx.callbackValue(f.Type)
				p.handlerType = qualified(op.output)
				op.outputFormat = "JSON Lines"
			// HuJSON is an API contract, not a property of the Go type any.
			case resource == "PolicyFile" && (op.name == "Set" || op.name == "Validate") && typ == "any":
				p.kind = "raw"
			default:
				var err error
				p.kind, err = idx.kind(f.Type, map[string]bool{})
				if err != nil {
					return fail(err)
				}
			}
			if _, ok := f.Type.(*ast.StarExpr); ok {
				p.required = false
			}
			if resource == "PolicyFile" && n.Name == "etag" || resource == "Keys" && op.name == "List" && n.Name == "all" {
				p.required = false
			}
			op.params = append(op.params, p)
		}
	}
	if err := validateFlags(op.params); err != nil {
		return fail(err)
	}
	return op, nil
}

// acronymCase rewrites mixed-case acronyms the camel-case splitter below would
// otherwise break apart (IPv6 -> i-pv6).
var acronymCase = strings.NewReplacer("OAuth", "Oauth", "IPv4", "Ipv4", "IPv6", "Ipv6")

// kebab preserves acronym runs: DNS -> dns, deviceID -> device-id.
func kebab(s string) string {
	s = acronymCase.Replace(s)
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1]) || i+1 < len(rs) && unicode.IsLower(rs[i+1]) && unicode.IsUpper(rs[i-1])) {
			b.WriteByte('-')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

//go:embed commands.go.gotmpl
var commandsTemplateSource string

var commandsTemplate = template.Must(template.New("commands").Funcs(template.FuncMap{"shapeLiteral": shapeLiteral}).Parse(commandsTemplateSource))

type resourceData struct {
	Use, Short, Long string
	Commands         []commandData
}

type commandData struct {
	Use, Short, Long string
	Resource, Method string
	Params           []parameterData
	Result           bool
	Schemas          commandSchemasData
}

type parameterData struct {
	Variable, Flag, Type, Kind string
	Shape                      *shape
	Required                   bool
	Options                    []parameterData
	Constructor, HandlerType   string
}

type commandSchemasData struct {
	Inputs map[string]*shape
	Output *outputSchemaData
}

type outputSchemaData struct {
	Shape  *shape
	Format string
}

func commandView(resourceName string, op operation) commandData {
	data := commandData{
		Use: kebab(op.name), Short: firstLine(op.doc), Long: op.doc,
		Resource: resourceName, Method: op.name, Result: op.result,
		Schemas: commandSchemasData{Inputs: map[string]*shape{}},
	}
	if op.outputShape != nil {
		data.Schemas.Output = &outputSchemaData{Shape: op.outputShape, Format: op.outputFormat}
	}
	for i, p := range op.params {
		data.Params = append(data.Params, parameterView(p, "arg"+strconv.Itoa(i), data.Schemas.Inputs))
	}
	return data
}

// Only reviewed closed enums are validated; other named strings remain open.
var closedStringEnums = map[string]bool{
	"ContactType":   true,
	"LogType":       true,
	"UserType":      true,
	"UserRole":      true,
	"IncludeFields": true,
}

type enumData struct {
	Type      string
	Constants []string
}

func emit(resources []resource, enums []enumData, inputs, outputs map[string]*shape) ([]byte, error) {
	data := make([]resourceData, 0, len(resources))
	for _, r := range resources {
		group := resourceData{Use: kebab(r.name), Short: firstLine(r.doc), Long: r.doc}
		for _, op := range r.ops {
			group.Commands = append(group.Commands, commandView(r.name, op))
		}
		data = append(data, group)
	}
	var b bytes.Buffer
	if err := commandsTemplate.Execute(&b, struct {
		Resources []resourceData
		Enums     []enumData
		Inputs    map[string]*shape
		Outputs   map[string]*shape
	}{data, enums, inputs, outputs}); err != nil {
		return nil, fmt.Errorf("execute commands template: %w", err)
	}
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated code: %w", err)
	}
	return out, nil
}

func firstLine(s string) string { return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0] }
