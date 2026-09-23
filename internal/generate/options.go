// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package generate

import (
	"fmt"
	"go/ast"
	"slices"
	"strconv"
	"strings"
)

// underlying follows local named types without accepting recursive aliases.
func (idx index) underlying(e ast.Expr) ast.Expr {
	seen := map[string]bool{}
	for {
		n, ok := e.(*ast.Ident)
		if !ok || seen[n.Name] || idx.types[n.Name] == nil {
			return e
		}
		seen[n.Name] = true
		e = idx.types[n.Name]
	}
}

func isVariadic(e ast.Expr) bool { _, ok := e.(*ast.Ellipsis); return ok }

func fieldTypes(fields *ast.FieldList) []ast.Expr {
	var result []ast.Expr
	if fields != nil {
		for _, f := range fields.List {
			for range max(1, len(f.Names)) {
				result = append(result, f.Type)
			}
		}
	}
	return result
}

// The supported streaming convention is one func(T) error callback on an
// error-only operation. Other callback contracts must be adapted explicitly.
func (idx index) callbackValue(e ast.Expr) ast.Expr {
	fn, ok := idx.underlying(e).(*ast.FuncType)
	if !ok {
		return nil
	}
	args, results := fieldTypes(fn.Params), fieldTypes(fn.Results)
	if len(args) != 1 || len(results) != 1 || typeString(results[0]) != "error" || isVariadic(args[0]) {
		return nil
	}
	return args[0]
}

func (idx index) optionParameter(p parameter) (parameter, error) {
	elem := p.expr.(*ast.Ellipsis).Elt
	fn, ok := idx.underlying(elem).(*ast.FuncType)
	if !ok || fn.Params.NumFields() != 1 || fn.Results.NumFields() != 0 {
		return p, fmt.Errorf("unsupported functional options %s", typeString(p.expr))
	}
	if _, ok := idx.underlying(fn.Params.List[0].Type).(*ast.StarExpr); !ok {
		return p, fmt.Errorf("unsupported functional options %s: expected func(*T)", typeString(p.expr))
	}
	p.kind, p.typ = "options", qualified(elem)
	for _, f := range idx.functions {
		results := fieldTypes(f.Type.Results)
		if !strings.HasPrefix(f.Name.Name, "With") || f.Name.Name == "With" || deprecated(f) || len(results) != 1 || typeString(results[0]) != typeString(elem) {
			continue
		}
		option := parameter{name: strings.TrimPrefix(f.Name.Name, "With"), constructor: f.Name.Name}
		args := fieldTypes(f.Type.Params)
		switch {
		case len(args) == 0:
			option.kind, option.typ = "switch", "bool"
		case len(args) == 1 && !isVariadic(args[0]):
			var err error
			option.kind, err = idx.kind(args[0], map[string]bool{})
			if err != nil {
				return p, fmt.Errorf("%s: %w", f.Name.Name, err)
			}
			option.expr, option.typ = args[0], qualified(args[0])
		case len(args) == 2 && typeString(args[0]) == "string" && typeString(args[1]) == "[]string":
			option.kind, option.typ = "key-values", "string=[]string"
		default:
			return p, fmt.Errorf("%s: unsupported option constructor signature", f.Name.Name)
		}
		p.options = append(p.options, option)
	}
	if len(p.options) == 0 {
		return p, fmt.Errorf("%s: no supported With* constructors", typeString(elem))
	}
	slices.SortFunc(p.options, func(a, b parameter) int { return strings.Compare(a.name, b.name) })
	return p, nil
}

func buildParameterShapes(builder *shapeBuilder, p *parameter) error {
	if p.kind == "json" {
		var err error
		p.shape, err = builder.shape(p.expr)
		if err != nil {
			return fmt.Errorf("--%s: %w", kebab(p.name), err)
		}
	}
	for i := range p.options {
		if err := buildParameterShapes(builder, &p.options[i]); err != nil {
			return err
		}
	}
	return nil
}

func validateFlags(params []parameter) error {
	seen := map[string]bool{"help": true, "tailnet": true}
	var visit func([]parameter) error
	visit = func(ps []parameter) error {
		for _, p := range ps {
			if p.kind == "handler" {
				continue
			}
			if p.kind == "options" {
				if err := visit(p.options); err != nil {
					return err
				}
				continue
			}
			flag := kebab(p.name)
			if seen[flag] {
				return fmt.Errorf("duplicate or reserved flag --%s", flag)
			}
			seen[flag] = true
		}
		return nil
	}
	return visit(params)
}

func parameterView(p parameter, variable string, schemas map[string]*shape) parameterData {
	flag := kebab(p.name)
	if p.kind == "json" {
		schemas[flag] = p.shape
	}
	data := parameterData{Variable: variable, Flag: flag, Type: p.typ, Kind: p.kind, Required: p.required, Shape: p.shape, Constructor: p.constructor, HandlerType: p.handlerType}
	for i, option := range p.options {
		data.Options = append(data.Options, parameterView(option, variable+"opt"+strconv.Itoa(i), schemas))
	}
	return data
}
