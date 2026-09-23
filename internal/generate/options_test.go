// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: 2026 Mitsuo HEIJO

package generate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const optionSDK = `package tailscale
import "context"
type Client struct { Tailnet string }
type ThingsResource struct{}
func(c *Client) Things()*ThingsResource{return &ThingsResource{}}
type settings struct{}
type Option func(*settings)
type Label string
type Request struct { Name string }
var Calls []string
var OptionCount int
func WithLabel(value Label) Option { Calls=append(Calls,"label="+string(value)); return func(*settings){} }
func WithEnabled(value bool) Option { if !value { Calls=append(Calls,"false") }; return func(*settings){} }
func WithFast() Option { Calls=append(Calls,"fast"); return func(*settings){} }
func WithMatch(key string, values []string) Option { Calls=append(Calls,key+"="+values[0]); return func(*settings){} }
func WithRequest(value Request) Option { Calls=append(Calls,value.Name); return func(*settings){} }
// Deprecated: no longer supported.
func WithOld(value string) Option { return nil }
type Unrelated func(*settings)
func WithIgnored(value string) Unrelated { return nil }
func(r *ThingsResource) Search(ctx context.Context, opts ...Option)error{OptionCount=len(opts);return nil}
func(r *ThingsResource) Find(ctx context.Context, opts ...Option)error{OptionCount=len(opts);return nil}
type Entry struct { Message string }
type Handler func(Entry) error
type HandlerAlias = Handler
func(r *ThingsResource) Watch(ctx context.Context, handler HandlerAlias)error{
 if err:=handler(Entry{"one"});err!=nil{return err};return handler(Entry{"two"})
}
func(r *ThingsResource) Follow(ctx context.Context, handler func(*Entry) error)error{return handler(&Entry{"pointer"})}
`

func generateOptionFixture(t *testing.T, source string) ([]byte, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sdk.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return Generate(dir)
}

func TestShapeDispatch(t *testing.T) {
	generated, err := generateOptionFixture(t, optionSDK)
	if err != nil {
		t.Fatal(err)
	}
	// Compile and execute against a different SDK surface: text assertions alone
	// would miss incorrectly qualified types or invalid constructor invocations.
	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "sdk"), 0700); err != nil {
		t.Fatal(err)
	}
	write("sdk/sdk.go", []byte(optionSDK))
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join("../..", name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "go.mod" {
			data = []byte(strings.Replace(string(data), "module github.com/johejo/tailctl", "module fixture", 1))
		}
		write(name, data)
	}
	replaceImport := func(data []byte) []byte {
		return []byte(strings.ReplaceAll(string(data), `"tailscale.com/client/tailscale/v2"`, `"fixture/sdk"`))
	}
	write("commands_gen.go", replaceImport(generated))
	for _, name := range []string{"cli.go", "json.go"} {
		data, err := os.ReadFile(filepath.Join("../cli", name))
		if err != nil {
			t.Fatal(err)
		}
		write(name, replaceImport(data))
	}
	write("fixture_test.go", []byte(`package cli
import("bytes";"errors";"strings";"testing";"fixture/sdk")
const envTailnet="TAILCTL_TAILNET"
func newClient(func(string)string, any)*tailscale.Client{return &tailscale.Client{}}
func TestOptions(t *testing.T){
 for _,method:=range []string{"search","find"}{
  tailscale.Calls=nil
  cmd:=newCommand(&tailscale.Client{})
  cmd.SetArgs([]string{"things",method,"--label","test","--enabled=false","--fast","--match","os=linux","--request",`+"`"+`{"Name":"request"}`+"`"+`})
  if err:=cmd.Execute();err!=nil{t.Fatal(err)}
  if tailscale.OptionCount!=5{t.Fatal("options not forwarded",tailscale.OptionCount)}
  if got:=strings.Join(tailscale.Calls,",");got!="false,fast,label=test,os=linux,request"{t.Fatal(got)}
 }
 tailscale.Calls=nil
 cmd:=newCommand(&tailscale.Client{});cmd.SetArgs([]string{"things","search"})
 if err:=cmd.Execute();err!=nil{t.Fatal(err)}
 if len(tailscale.Calls)!=0{t.Fatal("options called without flags")}
 cmd=newCommand(&tailscale.Client{});cmd.SetArgs([]string{"things","search","--fast=false"})
 if err:=cmd.Execute();err!=nil{t.Fatal(err)}
 if len(tailscale.Calls)!=0{t.Fatal("disabled switch called constructor")}
 for _,flag:=range []string{"old","ignored"}{
  cmd=newCommand(&tailscale.Client{});cmd.SetArgs([]string{"things","search","--"+flag,"x"})
  if err:=cmd.Execute();err==nil{t.Fatal("unexpected flag",flag)}
 }
}
func TestStreaming(t *testing.T){
 for _,method:=range []string{"watch","follow"}{
  cmd:=newCommand(&tailscale.Client{});var out bytes.Buffer;cmd.SetOut(&out);cmd.SetArgs([]string{"things",method})
  if err:=cmd.Execute();err!=nil{t.Fatal(err)}
  want:="{\"Message\":\"one\"}\n{\"Message\":\"two\"}\n"
  if method=="follow"{want="{\"Message\":\"pointer\"}\n"}
  if out.String()!=want{t.Fatal(out.String())}
 }
 cmd:=newCommand(&tailscale.Client{});cmd.SetOut(failedWriter{});cmd.SetArgs([]string{"things","watch"})
 if err:=cmd.Execute();!errors.Is(err,errWrite){t.Fatal(err)}
}
var errWrite=errors.New("write failed")
type failedWriter struct{}
func(failedWriter)Write([]byte)(int,error){return 0,errWrite}
func TestOptionSchema(t *testing.T){
 tailscale.Calls=nil
 cmd:=newCommand(&tailscale.Client{});var out bytes.Buffer;cmd.SetOut(&out);cmd.SetArgs([]string{"things","search","schema", "--input", "request"})
 if err:=cmd.Execute();err!=nil{t.Fatal(err)}
 if !strings.Contains(out.String(),"Name")||len(tailscale.Calls)!=0{t.Fatal(out.String(),tailscale.Calls)}
 cmd=newCommand(&tailscale.Client{});cmd.SetArgs([]string{"things","search","--request",`+"`"+`{"Unknown":1}`+"`"+`})
 if err:=cmd.Execute();err==nil{t.Fatal("invalid option JSON accepted")}
}
`))
	cmd := exec.Command("go", "test", "-mod=readonly", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v\n%s", err, output)
	}
}

func TestUnsupportedOptionAndHandlerShapes(t *testing.T) {
	for _, tt := range []struct{ name, source, want string }{
		{"missing constructors", `type Option func(*string);func(r *ThingsResource) Run(ctx context.Context,opts ...Option)error{return nil}`, "no supported With* constructors"},
		{"unsupported constructor", `type Option func(*string);func WithPair(a,b string)Option{return nil};func(r *ThingsResource) Run(ctx context.Context,opts ...Option)error{return nil}`, "WithPair: unsupported option constructor"},
		{"flag collision", `type Option func(*string);func WithName(string)Option{return nil};func(r *ThingsResource) Run(ctx context.Context,name string,opts ...Option)error{return nil}`, "duplicate or reserved flag --name"},
		{"reserved flag", `type Option func(*string);func WithTailnet(string)Option{return nil};func(r *ThingsResource) Run(ctx context.Context,opts ...Option)error{return nil}`, "duplicate or reserved flag --tailnet"},
		{"multiple handlers", `func(r *ThingsResource) Run(ctx context.Context,a,b func(string)error)error{return nil}`, "single callback"},
		{"handler and result", `func(r *ThingsResource) Run(ctx context.Context,h func(string)error)(string,error){return "",nil}`, "error-only result"},
		{"unknown any", `func(r *ThingsResource) Run(ctx context.Context,body any)error{return nil}`, "unsupported input type any"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := generateOptionFixture(t, `package sdk;type Client struct{};type ThingsResource struct{};func(c *Client)Things()*ThingsResource{return nil};`+tt.source)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("got %v; want %s", err, tt.want)
			}
		})
	}
}
