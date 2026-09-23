// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// jsonShape describes a JSON representation, not API business requirements.
// Definitions and their references are generated from the SDK.
type jsonShape struct {
	Kind, Ref, Format, Integer, Key string
	Nullable, Fixed                 bool
	Length                          int
	FloatBits                       int
	Elem                            *jsonShape
	Fields                          []jsonField
	Enum                            []string
}

type jsonField struct {
	Name, Description string
	MayOmit           bool
	Shape             *jsonShape
}

// jsonDirection distinguishes the decoding rules applied to input from the
// encoding rules the SDK uses for output; the two models are generated apart.
type jsonDirection uint8

const (
	decode jsonDirection = iota
	encode
)

// schemaRegistry resolves named shape references for one direction. Carrying
// the direction alongside the definitions keeps the two from being mismatched.
type schemaRegistry struct {
	direction jsonDirection
	types     map[string]*jsonShape
}

func readJSONInput[T any](cmd *cobra.Command, name string, shape *jsonShape) (T, error) {
	var result T
	if !cmd.Flags().Changed(name) {
		return result, nil
	}
	data, err := readContent(cmd, name)
	if err == nil {
		err = inputSchemas.validate(shape, data)
	}
	if err == nil {
		err = json.Unmarshal(data, &result)
	}
	if err != nil {
		return result, fmt.Errorf("--%s: %w", name, err)
	}
	return result, nil
}

func (r schemaRegistry) validate(s *jsonShape, data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	value, err := decodeJSONValue(dec, "$", 0)
	if err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("$: expected a single JSON value")
	}
	return r.check(s, value, "$")
}

// Reject duplicate keys before decoding into an SDK struct: encoding/json merges
// repeated object fields, whereas a map would retain only the last occurrence.
func decodeJSONValue(dec *json.Decoder, path string, depth int) (any, error) {
	if depth > 10000 {
		return nil, fmt.Errorf("%s: JSON nesting too deep", path)
	}
	token, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			key, ok := token.(string)
			if !ok {
				return nil, fmt.Errorf("%s: expected object key", path)
			}
			childPath := jsonPath(path, key)
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("%s: duplicate field", childPath)
			}
			value, err := decodeJSONValue(dec, childPath, depth+1)
			if err != nil {
				return nil, err
			}
			object[key] = value
		}
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return object, nil
	case json.Delim('['):
		array := []any{}
		for dec.More() {
			value, err := decodeJSONValue(dec, fmt.Sprintf("%s[%d]", path, len(array)), depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := dec.Token(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return array, nil
	default:
		return token, nil
	}
}

func (r schemaRegistry) check(s *jsonShape, value any, path string) error {
	if value == nil && s.Nullable {
		return nil
	}
	if s.Ref != "" {
		return r.check(r.types[s.Ref], value, path)
	}
	if s.Kind == "any" {
		return nil
	}
	bad := func() error { return fmt.Errorf("%s: expected %s, got %s", path, s.label(), jsonValueType(value)) }
	switch s.Kind {
	case "map":
		object, ok := value.(map[string]any)
		if !ok {
			return bad()
		}
		for _, key := range slices.Sorted(maps.Keys(object)) {
			childPath := jsonPath(path, key)
			if s.Key != "string" {
				if err := checkInteger(key, s.Key); err != nil {
					return fmt.Errorf("%s: invalid %s map key", childPath, s.Key)
				}
			}
			if err := r.check(s.Elem, object[key], childPath); err != nil {
				return err
			}
		}
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			return bad()
		}
		for _, key := range slices.Sorted(maps.Keys(object)) {
			childPath := jsonPath(path, key)
			field := s.field(key)
			if field == nil {
				for _, f := range s.Fields {
					if strings.EqualFold(f.Name, key) {
						return fmt.Errorf("%s: unknown field; did you mean %q?", childPath, f.Name)
					}
				}
				return fmt.Errorf("%s: unknown field", childPath)
			}
			if err := r.check(field, object[key], childPath); err != nil {
				return err
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			return bad()
		}
		if s.Fixed && len(array) != s.Length {
			return fmt.Errorf("%s: expected %d items, got %d", path, s.Length, len(array))
		}
		for i, v := range array {
			if err := r.check(s.Elem, v, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			return bad()
		}
		if err := validateEnum(text, s.Enum); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		switch s.Format {
		case "date-time", "sdk-time":
			if s.Format == "sdk-time" && text == "" {
				return nil
			}
			// Use the same decoder as the SDK, including its RFC3339 checks.
			data, _ := json.Marshal(text)
			var t time.Time
			if err := t.UnmarshalJSON(data); err != nil {
				return fmt.Errorf("%s: expected RFC3339 date-time: %w", path, err)
			}
		case "ssh-check-period":
			if text == "always" || text == "" {
				return nil
			}
			if _, err := time.ParseDuration(text); err != nil {
				return fmt.Errorf("%s: expected Go duration or \"always\": %w", path, err)
			}
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return bad()
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return bad()
		}
		if err := checkInteger(string(n), s.Integer); err != nil {
			return fmt.Errorf("%s: expected %s integer: %w", path, s.Integer, err)
		}
	case "number":
		n, ok := value.(json.Number)
		if !ok {
			return bad()
		}
		bits := s.FloatBits
		if bits == 0 {
			bits = 64
		}
		if _, err := strconv.ParseFloat(string(n), bits); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	default:
		return fmt.Errorf("%s: unsupported generated JSON kind %q", path, s.Kind)
	}
	return nil
}

func (s *jsonShape) field(name string) *jsonShape {
	for _, f := range s.Fields {
		if f.Name == name {
			return f.Shape
		}
	}
	return nil
}

func integerBits(kind string) int {
	bits := strconv.IntSize
	switch kind {
	case "int8", "uint8", "byte":
		bits = 8
	case "int16", "uint16":
		bits = 16
	case "int32", "uint32", "rune":
		bits = 32
	case "int64", "uint64":
		bits = 64
	}
	return bits
}

func unsignedInteger(kind string) bool { return strings.HasPrefix(kind, "uint") || kind == "byte" }

func checkInteger(text, kind string) error {
	bits := integerBits(kind)
	if unsignedInteger(kind) {
		_, err := strconv.ParseUint(text, 10, bits)
		return err
	}
	_, err := strconv.ParseInt(text, 10, bits)
	return err
}

func jsonValueType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func jsonPath(path, key string) string {
	for i, r := range key {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			return path + "[" + strconv.Quote(key) + "]"
		}
	}
	if key == "" {
		return path + `[""]`
	}
	return path + "." + key
}

// outputSchema describes what a command writes to stdout.
type outputSchema struct {
	Format string
	Shape  *jsonShape
}

// commandSchemas is everything the generator knows about one command's JSON.
type commandSchemas struct {
	Inputs map[string]*jsonShape
	Output *outputSchema
}

// configureCommandSchemas adds a schema subcommand and documents JSON input and output.
// The descriptions are large and usually unread, so they are rendered only when
// help actually runs; help is the sole consumer of the annotations.
func configureCommandSchemas(cmd *cobra.Command, schemas commandSchemas) {
	addSchemaCommand(cmd, schemas)
	help := cmd.HelpFunc()
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		describeSchemas(cmd, schemas)
		help(cmd, args)
	})
}

func setAnnotation(cmd *cobra.Command, name, value string) {
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[name] = value
}

func describeSchemas(cmd *cobra.Command, schemas commandSchemas) {
	if len(schemas.Inputs) > 0 {
		var help strings.Builder
		help.WriteString("\nJSON inputs:\n")
		seen := map[string]bool{}
		choices := slices.Sorted(maps.Keys(schemas.Inputs))
		for _, name := range choices {
			shape := schemas.Inputs[name]
			fmt.Fprintf(&help, "  --%s <%s>\n", name, shape.label())
			inputSchemas.help(shape, &help, "    ", seen)
		}
		input := choices[0]
		if len(choices) > 1 {
			input = "<" + strings.Join(choices, "|") + ">"
		}
		fmt.Fprintf(&help, "\n  Fields may be omitted; API-specific requirements may still apply.\n\n  Show JSON Schema:\n    %s schema --input %s\n", cmd.CommandPath(), input)
		setAnnotation(cmd, "json-inputs", help.String())
	}
	if schemas.Output != nil {
		var help strings.Builder
		fmt.Fprintf(&help, "\nOutput:\n  %s <%s>", schemas.Output.Format, schemas.Output.Shape.label())
		if schemas.Output.Format == "JSON Lines" {
			help.WriteString(" (one value per line)")
		}
		help.WriteByte('\n')
		outputSchemas.help(schemas.Output.Shape, &help, "    ", map[string]bool{})
		setAnnotation(cmd, "json-output", help.String())
	}
}

// addSchemaCommand exports the explicitly selected output or JSON input schema.
func addSchemaCommand(parent *cobra.Command, schemas commandSchemas) {
	choices := slices.Sorted(maps.Keys(schemas.Inputs))
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Print input or output JSON Schema",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, _ := cmd.Flags().GetString("input")
			registry, shape := inputSchemas, schemas.Inputs[name]
			if cmd.Flags().Changed("output") {
				if schemas.Output == nil {
					return fmt.Errorf("schema: command has no JSON output")
				}
				registry, shape = outputSchemas, schemas.Output.Shape
			} else if shape == nil {
				return fmt.Errorf("schema: unknown JSON input %q; choose %s", name, strings.Join(choices, ", "))
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(registry.schemaDocument(shape))
		},
	}
	cmd.Flags().Bool("output", false, "Show output JSON Schema")
	if len(choices) > 0 {
		cmd.Flags().String("input", "", "Show input JSON Schema for a flag (values: "+strings.Join(choices, ", ")+")")
		cmd.MarkFlagsOneRequired("input", "output")
		cmd.MarkFlagsMutuallyExclusive("input", "output")
	} else {
		cmd.MarkFlagsOneRequired("output")
	}
	// Keep schema help independent of the parent's API input/output descriptions.
	cmd.SetHelpFunc(parent.HelpFunc())
	parent.AddCommand(cmd)
}

func (s *jsonShape) label() string {
	if s.Nullable {
		nonNull := *s
		nonNull.Nullable = false
		return nonNull.label() + " | null"
	}
	if s.Ref != "" {
		return s.Ref
	}
	var label string
	switch s.Kind {
	case "array":
		label = "(" + s.Elem.label() + ")[]"
		if s.Fixed {
			label = fmt.Sprintf("(%s)[%d]", s.Elem.label(), s.Length)
		}
	case "map":
		label = "map[" + s.Key + "]" + s.Elem.label()
	default:
		label = s.Kind
	}
	if s.Format != "" {
		label += " (" + s.Format + ")"
	}
	if len(s.Enum) > 0 {
		label += " {" + strings.Join(s.Enum, ", ") + "}"
	}
	return label
}

func (r schemaRegistry) help(s *jsonShape, b *strings.Builder, indent string, seen map[string]bool) {
	if s.Ref != "" {
		if seen[s.Ref] {
			return
		}
		seen[s.Ref] = true
		fmt.Fprintf(b, "%s%s:\n", indent, s.Ref)
		r.help(r.types[s.Ref], b, indent+"  ", seen)
		return
	}
	if s.Elem != nil {
		r.help(s.Elem, b, indent, seen)
		return
	}
	if s.Kind != "object" {
		fmt.Fprintf(b, "%s%s\n", indent, s.label())
		return
	}
	for _, f := range s.Fields {
		fmt.Fprintf(b, "%s%s  %s", indent, f.Name, f.Shape.label())
		if f.MayOmit {
			b.WriteString(" (may be omitted)")
		}
		if f.Description != "" {
			fmt.Fprintf(b, " — %s", strings.Join(strings.Fields(f.Description), " "))
		}
		b.WriteByte('\n')
		if f.Shape.Ref != "" || f.Shape.Elem != nil || f.Shape.Kind == "object" {
			r.help(f.Shape, b, indent+"  ", seen)
		}
	}
}

func (r schemaRegistry) schemaDocument(s *jsonShape) map[string]any {
	defs := map[string]any{}
	root := r.schema(s, defs)
	root["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	if len(defs) > 0 {
		root["$defs"] = defs
	}
	return root
}

func (r schemaRegistry) schema(s *jsonShape, defs map[string]any) map[string]any {
	if s.Nullable {
		nonNull := *s
		nonNull.Nullable = false
		out := r.schema(&nonNull, defs)
		// A type union is sufficient unless an enum or a reference also
		// constrains the value, or the schema uses a composite representation.
		if kind, ok := out["type"].(string); ok && len(s.Enum) == 0 {
			out["type"] = []string{kind, "null"}
			return out
		}
		return map[string]any{"anyOf": []any{out, map[string]any{"type": "null"}}}
	}
	if s.Ref != "" {
		if _, ok := defs[s.Ref]; !ok {
			defs[s.Ref] = nil
			defs[s.Ref] = r.schema(r.types[s.Ref], defs)
		}
		return map[string]any{"$ref": "#/$defs/" + s.Ref}
	}
	if s.Kind == "any" {
		return map[string]any{}
	}
	kind := s.Kind
	if kind == "map" {
		kind = "object"
	}
	out := map[string]any{"type": kind}
	switch s.Kind {
	case "object":
		props := map[string]any{}
		var required []string
		for _, f := range s.Fields {
			p := r.schema(f.Shape, defs)
			if f.Description != "" {
				p["description"] = f.Description
			}
			props[f.Name] = p
			if r.direction == encode && !f.MayOmit {
				required = append(required, f.Name)
			}
		}
		if len(required) > 0 {
			out["required"] = required
		}
		out["properties"] = props
		out["additionalProperties"] = false
	case "map":
		out["additionalProperties"] = r.schema(s.Elem, defs)
		if s.Key != "string" {
			pattern := `^[+-]?[0-9]+$`
			if unsignedInteger(s.Key) {
				pattern = `^[0-9]+$`
			}
			out["propertyNames"] = map[string]any{"pattern": pattern}
		}
	case "integer":
		bits := integerBits(s.Integer)
		if unsignedInteger(s.Integer) {
			out["minimum"] = 0
			out["maximum"] = ^uint64(0) >> uint(64-bits)
		} else {
			maximum := int64(^uint64(0) >> uint(65-bits))
			out["minimum"] = -maximum - 1
			out["maximum"] = maximum
		}
	case "array":
		out["items"] = r.schema(s.Elem, defs)
		if s.Fixed {
			out["minItems"] = s.Length
			out["maxItems"] = s.Length
		}
	}
	if len(s.Enum) > 0 {
		out["enum"] = s.Enum
	}
	switch s.Format {
	case "date-time":
		out["format"] = "date-time"
	case "base64":
		out["contentEncoding"] = "base64"
	case "nanoseconds":
		out["x-tailctl-format"] = "nanoseconds"
	case "sdk-time":
		delete(out, "type")
		out["anyOf"] = []any{map[string]any{"type": "string", "format": "date-time"}, map[string]any{"const": ""}}
	case "ssh-check-period":
		out["description"] = "Go duration string, empty string, or always"
		out["x-tailctl-format"] = "ssh-check-period"
	}
	return out
}
