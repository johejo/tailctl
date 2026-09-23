// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package generate

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func generateFixture(t *testing.T, declarations string) ([]byte, error) {
	t.Helper()
	source := `package sdk
 type Client struct{}
 type ThingsResource struct{}
 func(c *Client) Things()*ThingsResource{return nil}
 func(r *ThingsResource) Create(ctx context.Context, request *Request)error{return nil}
 ` + declarations
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sdk.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := Generate(dir)
	if err == nil {
		again, err := Generate(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(result, again) {
			t.Fatal("generation is not deterministic")
		}
	}
	return result, err
}

func TestJSONModelGeneration(t *testing.T) {
	result, err := generateFixture(t, `type ContactType string
 const ContactSecurity ContactType = "security"
 type Request struct {
  // Child points to another request.
  Child *Request `+"`json:\"child,omitempty\"`"+`
  Roles []ContactType `+"`json:\"roles\"`"+`
  ByName map[string]*Request `+"`json:\"byName\"`"+`
  Numbers [2]int64
  Created time.Time
  Payload any
  Hidden func() `+"`json:\"-\"`"+`
  private func()
 }`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`readJSONInput[*tailscale.Request](cmd, "request", &jsonShape{Ref: "Request", Nullable: true})`,
		`"request": &jsonShape{Ref: "Request", Nullable: true}`,
		`Description: "Child points to another request."`,
		`Name: "child"`,
		`Name: "Numbers"`,
		`Length: 2, Fixed: true`,
		`Format: "date-time"`,
		`Kind: "any"`,
		`Enum: []string{string(tailscale.ContactSecurity)}`,
	} {
		if !strings.Contains(string(result), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(string(result), `Name: "Hidden"`) || strings.Contains(string(result), `Name: "private"`) {
		t.Fatal("non-JSON field included")
	}
	if strings.Count(string(result), `"Request": &jsonShape`) != 1 {
		t.Fatal("recursive definition not deduplicated")
	}
}

func TestUnsupportedJSONContracts(t *testing.T) {
	for _, tt := range []struct{ decl, want string }{
		{`type Request struct{Value external.Type}`, "unsupported external JSON type external.Type"},
		{`type Request struct{Value Custom};type Custom string;func(c *Custom)UnmarshalJSON([]byte)error{return nil}`, "unsupported UnmarshalJSON"},
		{`type Request struct{Value Custom};type Custom string;func(c *Custom)UnmarshalText([]byte)error{return nil}`, "unsupported UnmarshalText"},
		{`type Request struct{Value []byte}`, "unsupported JSON byte array"},
		{`type Byte uint8;type Request struct{Value []Byte}`, "unsupported JSON byte array"},
		{`type Request struct{Value map[bool]string}`, "unsupported JSON map key"},
		{`type Request struct{Embedded};type Embedded struct{Value string}`, "unsupported embedded JSON field"},
		{"type Request struct{Value int `json:\"value,string\"`}", "unsupported JSON tag option"},
		{"type Request struct{A string `json:\"same\"`;B string `json:\"same\"`}", "duplicate JSON field"},
	} {
		t.Run(tt.want, func(t *testing.T) {
			_, err := generateFixture(t, tt.decl)
			if err == nil || !strings.Contains(err.Error(), "Things.Create --request:") || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("%v", err)
			}
		})
	}
}

func TestOutputModelGeneration(t *testing.T) {
	result, err := generateFixture(t, `type Request struct{ Created Time }
 type Time struct{time.Time}
 func(t Time)MarshalJSON()([]byte,error){return nil,nil}
 func(t *Time)UnmarshalJSON([]byte)error{return nil}
 type Reply struct {
  Next *Reply `+"\x60json:\"next,omitempty\"\x60"+`
  Zero int `+"\x60json:\"zero,omitzero\"\x60"+`
  Created Time
  Duration time.Duration
  Bytes []byte
  Hidden string `+"\x60json:\"-\"\x60"+`
 }
 func(r *ThingsResource)Get(ctx context.Context)(*Reply,error){return nil,nil}
 `)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`Output: &outputSchema{Format: "JSON", Shape: &jsonShape{Ref: "Reply", Nullable: true}}`,
		`Format: "sdk-time"`, `Format: "date-time"`,
		`Format: "nanoseconds"`, `Format: "base64", Nullable: true`,
		`MayOmit: true`,
	} {
		if !strings.Contains(string(result), want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Count(string(result), `"Reply": &jsonShape`) != 1 {
		t.Fatal("recursive output definition not deduplicated")
	}
	if strings.Contains(string(result), `Name: "Hidden"`) {
		t.Fatal("hidden output field included")
	}
}

func TestOutputRejectsUnknownMarshal(t *testing.T) {
	for _, method := range []string{"MarshalJSON", "MarshalText"} {
		_, err := generateFixture(t, `type Request struct{}
 type Reply string
 func(r Reply)`+method+`()([]byte,error){return nil,nil}
 func(r *ThingsResource)Get(ctx context.Context)(Reply,error){return "",nil}
 `)
		if err == nil || !strings.Contains(err.Error(), "Things.Get output: JSON type Reply has unsupported "+method) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestOutputAllowsDecodeOnlyType(t *testing.T) {
	_, err := generateFixture(t, `type Request struct{}
 type Reply string
 func(r *Reply)UnmarshalJSON([]byte)error{return nil}
 func(r *ThingsResource)Get(ctx context.Context)(Reply,error){return "",nil}
 `)
	if err != nil {
		t.Fatal(err)
	}
}

// One recursive SDK type used in both directions must retain distinct contracts.
func TestShapeDirections(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "sdk.go", `package sdk
 type ContactType string
 type Time struct{}
 type Node struct {
  Next *Node `+"`json:\"next,omitempty\"`"+`
  Created Time
  Role ContactType
 }
 `, 0)
	if err != nil {
		t.Fatal(err)
	}
	idx := index{types: map[string]ast.Expr{}, enums: map[string][]string{"ContactType": {"ContactSecurity"}}}
	for _, decl := range f.Decls {
		for _, spec := range decl.(*ast.GenDecl).Specs {
			typ := spec.(*ast.TypeSpec)
			idx.types[typ.Name.Name] = typ.Type
		}
	}
	for _, direction := range []jsonDirection{decode, encode} {
		name := "decode"
		if direction == encode {
			name = "encode"
		}
		t.Run(name, func(t *testing.T) {
			builder := newShapeBuilder(idx, direction)
			root, err := builder.shape(ast.NewIdent("Node"))
			if err != nil {
				t.Fatal(err)
			}
			if root.Ref != "Node" || len(builder.defs) != 3 {
				t.Fatalf("unexpected definitions: %+v", builder.defs)
			}
			next := builder.defs["Node"].Fields[0]
			if !next.Shape.Nullable || next.Shape.Ref != "Node" {
				t.Fatalf("recursive reference lost: %+v", next)
			}
			if next.MayOmit != (direction == encode) {
				t.Fatalf("wrong omission rule: %+v", next)
			}
			format := "sdk-time"
			var enum []string
			if direction == encode {
				format = "date-time"
			} else {
				enum = []string{"ContactSecurity"}
			}
			if got := builder.defs["Time"].Format; got != format {
				t.Fatalf("Time format = %q, want %q", got, format)
			}
			if got := builder.defs["ContactType"].Enum; !slices.Equal(got, enum) {
				t.Fatalf("enum = %v, want %v", got, enum)
			}
		})
	}
}
