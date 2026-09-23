// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package generate

import (
	"fmt"
	"go/ast"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// shapeBuilder builds a shared description for help, schema export and validation.
// Named references keep recursive types finite and definitions deduplicated.
type shapeBuilder struct {
	idx       index
	defs      map[string]*shape
	direction jsonDirection
}

type jsonDirection uint8

const (
	decode jsonDirection = iota
	encode
)

func newShapeBuilder(idx index, direction jsonDirection) *shapeBuilder {
	return &shapeBuilder{idx: idx, defs: map[string]*shape{}, direction: direction}
}

func (d jsonDirection) customMethod(name string) bool {
	if d == encode {
		return name == "MarshalJSON" || name == "MarshalText"
	}
	return name == "UnmarshalJSON" || name == "UnmarshalText"
}

// shape builds a JSON representation without emitting Go source.
func (m *shapeBuilder) shape(e ast.Expr) (*shape, error) {
	switch t := e.(type) {
	case *ast.Ident:
		switch t.Name {
		case "any":
			return &shape{Kind: "any"}, nil
		case "string":
			return &shape{Kind: "string"}, nil
		case "bool":
			return &shape{Kind: "boolean"}, nil
		case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "byte", "rune":
			return &shape{Kind: "integer", Integer: t.Name}, nil
		case "float32", "float64":
			bits, _ := strconv.Atoi(strings.TrimPrefix(t.Name, "float"))
			return &shape{Kind: "number", FloatBits: bits}, nil
		}
		if _, ok := m.defs[t.Name]; !ok {
			underlying, ok := m.idx.types[t.Name]
			if !ok {
				return nil, fmt.Errorf("unknown JSON type %s", t.Name)
			}
			m.defs[t.Name] = nil // recursion guard
			var body *shape
			var err error
			switch t.Name {
			case "Time":
				body = &shape{Kind: "string", Format: "sdk-time"}
				if m.direction == encode {
					body = &shape{Kind: "string", Format: "date-time"}
				}
			case "SSHCheckPeriod":
				body = &shape{Kind: "string", Format: "ssh-check-period"}
			default:
				for _, method := range m.idx.methods[t.Name] {
					if m.direction.customMethod(method.Name.Name) {
						return nil, fmt.Errorf("JSON type %s has unsupported %s", t.Name, method.Name.Name)
					}
				}
				body, err = m.shape(underlying)
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", t.Name, err)
			}
			if m.direction == decode && closedStringEnums[t.Name] {
				body.Enum = slices.Clone(m.idx.enums[t.Name])
				slices.Sort(body.Enum)
			}
			m.defs[t.Name] = body
		}
		return &shape{Ref: t.Name}, nil
	case *ast.StarExpr:
		child, err := m.shape(t.X)
		if err != nil {
			return nil, err
		}
		child.Nullable = true
		return child, nil
	case *ast.SelectorExpr:
		if m.direction == encode && typeString(t) == "time.Duration" {
			return &shape{Kind: "integer", Integer: "int64", Format: "nanoseconds"}, nil
		}
		if typeString(t) == "time.Time" {
			return &shape{Kind: "string", Format: "date-time"}, nil
		}
		return nil, fmt.Errorf("unsupported external JSON type %s", typeString(t))
	case *ast.InterfaceType:
		if len(t.Methods.List) == 0 {
			return &shape{Kind: "any"}, nil
		}
	case *ast.ArrayType:
		if t.Len == nil && m.isByte(t.Elt, map[string]bool{}) {
			if m.direction == encode {
				return &shape{Kind: "string", Format: "base64", Nullable: true}, nil
			}
			return nil, fmt.Errorf("unsupported JSON byte array %s", typeString(t))
		}
		child, err := m.shape(t.Elt)
		if err != nil {
			return nil, err
		}
		body := &shape{Kind: "array", Elem: child}
		if t.Len == nil {
			body.Nullable = true
		} else {
			n, err := strconv.Atoi(typeString(t.Len))
			if err != nil || n < 0 {
				return nil, fmt.Errorf("unsupported array length %s", typeString(t.Len))
			}
			body.Length, body.Fixed = n, true
		}
		return body, nil
	case *ast.MapType:
		key := typeString(t.Key)
		switch key {
		case "string", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64":
		default:
			return nil, fmt.Errorf("unsupported JSON map key %s", key)
		}
		child, err := m.shape(t.Value)
		return &shape{Kind: "map", Nullable: true, Key: key, Elem: child}, err
	case *ast.StructType:
		var fields []shapeField
		seen := map[string]bool{}
		for _, f := range t.Fields.List {
			tag := ""
			if f.Tag != nil {
				raw, err := strconv.Unquote(f.Tag.Value)
				if err != nil {
					return nil, err
				}
				tag = reflect.StructTag(raw).Get("json")
			}
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if len(f.Names) == 0 {
				return nil, fmt.Errorf("unsupported embedded JSON field %s", typeString(f.Type))
			}
			for _, n := range f.Names {
				if !n.IsExported() {
					continue
				}
				name := parts[0]
				if name == "" {
					name = n.Name
				}
				if seen[name] {
					return nil, fmt.Errorf("duplicate JSON field %s", name)
				}
				seen[name] = true
				for _, option := range parts[1:] {
					if option != "omitempty" && option != "omitzero" && option != "" {
						return nil, fmt.Errorf("unsupported JSON tag option %q", option)
					}
				}
				child, err := m.shape(f.Type)
				if err != nil {
					return nil, fmt.Errorf("field %s: %w", name, err)
				}
				fields = append(fields, shapeField{
					Name:        name,
					Description: strings.TrimSpace(f.Doc.Text() + f.Comment.Text()),
					Shape:       child,
					MayOmit:     m.direction == encode && (slices.Contains(parts[1:], "omitempty") || slices.Contains(parts[1:], "omitzero")),
				})
			}
		}
		if len(fields) == 0 {
			return &shape{Kind: "object"}, nil
		}
		return &shape{Kind: "object", Fields: fields}, nil
	}
	return nil, fmt.Errorf("unsupported JSON type %s", typeString(e))
}

// Byte slices use base64 encoding even when the element has a named uint8 type.
func (m *shapeBuilder) isByte(e ast.Expr, seen map[string]bool) bool {
	ident, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	if ident.Name == "byte" || ident.Name == "uint8" {
		return true
	}
	if seen[ident.Name] {
		return false
	}
	seen[ident.Name] = true
	return m.isByte(m.idx.types[ident.Name], seen)
}
