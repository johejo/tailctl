// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package generate

import (
	"fmt"
	"strings"
)

// shape is the generator's JSON representation, independent of Go source.
// Enum contains SDK constant names, whose values are resolved in generated code.
// Ref keeps named and recursive definitions finite.
type shape struct {
	Kind, Ref, Format, Integer, Key string
	Nullable, Fixed                 bool
	Length, FloatBits               int
	Elem                            *shape
	Fields                          []shapeField
	Enum                            []string
}

type shapeField struct {
	Name, Description string
	MayOmit           bool
	Shape             *shape
}

// shapeLiteral is the emission boundary between the model and CLI Go code.
func shapeLiteral(s *shape) string {
	if s == nil {
		return "nil"
	}
	var fields []string
	addString := func(name, value string) {
		if value != "" {
			fields = append(fields, fmt.Sprintf("%s:%q", name, value))
		}
	}
	addString("Kind", s.Kind)
	addString("Ref", s.Ref)
	addString("Integer", s.Integer)
	addString("Format", s.Format)
	if s.Elem != nil {
		fields = append(fields, "Elem:"+shapeLiteral(s.Elem))
	}
	if s.Nullable {
		fields = append(fields, "Nullable:true")
	}
	addString("Key", s.Key)
	if s.Fixed {
		fields = append(fields, fmt.Sprintf("Length:%d, Fixed:true", s.Length))
	}
	if s.FloatBits != 0 {
		fields = append(fields, fmt.Sprintf("FloatBits:%d", s.FloatBits))
	}
	if len(s.Fields) > 0 {
		var b strings.Builder
		b.WriteString("Fields:[]jsonField{\n")
		for _, f := range s.Fields {
			fmt.Fprintf(&b, "{Name:%q,", f.Name)
			if f.Description != "" {
				fmt.Fprintf(&b, "Description:%q,", f.Description)
			}
			fmt.Fprintf(&b, "Shape:%s,", shapeLiteral(f.Shape))
			if f.MayOmit {
				b.WriteString("MayOmit:true,")
			}
			b.WriteString("},\n")
		}
		b.WriteString("}")
		fields = append(fields, b.String())
	}
	if len(s.Enum) > 0 {
		var b strings.Builder
		b.WriteString("Enum:[]string{")
		for _, name := range s.Enum {
			fmt.Fprintf(&b, "string(tailscale.%s),", name)
		}
		b.WriteString("}")
		fields = append(fields, b.String())
	}
	return "&jsonShape{" + strings.Join(fields, ",") + "}"
}
