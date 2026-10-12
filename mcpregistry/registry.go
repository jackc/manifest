// Package mcpregistry pairs MCP tool registration with inspectable declarations.
// Applications own exposure, authentication, error policy, and transport setup.
// Construction is single-threaded; Server freezes registration before serving.
package mcpregistry

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Stage prepares a call's context or refuses the call. Stages run in declaration
// order, before application handlers. For typed tools the SDK validates and
// decodes arguments first. These are tool-call stages, not HTTP authentication.
// A stage's name describes its installed function, not proof of its behavior.
type Stage struct {
	Name    string
	Prepare func(context.Context, *mcp.CallToolRequest) (context.Context, error)
}

type Spec[M any] struct {
	Tool     mcp.Tool `json:"tool"`
	Metadata M        `json:"metadata"`
}

// Compiler interprets application metadata. It must not perform I/O or mutate
// the declaration. Global stages run before those returned by the compiler.
type Compiler[M any] func(Spec[M]) ([]Stage, error)

type BindingDescription struct {
	Namespace         string `json:"namespace,omitempty"`
	Action            string `json:"action"`
	Description       string `json:"description"`
	Errors            string `json:"error_policy"`
	Result            string `json:"result_policy"`
	MaxArgumentsBytes int    `json:"max_arguments_bytes"`
}

type Description struct {
	Kind      string              `json:"kind"`
	Arguments string              `json:"arguments"`
	Binding   *BindingDescription `json:"binding,omitempty"`
	Gaps      []string            `json:"gaps"`
}

type Entry[M any] struct {
	Spec[M]
	Description
	Stages []string `json:"stages"`
}

type Inventory[M any] struct {
	SchemaVersion int        `json:"schema_version"`
	Tools         []Entry[M] `json:"tools"`
}

// Endpoint can only be built by Bind, BindRaw, Typed, or Custom. Its handler
// and description cannot be supplied independently by registration callers.
type Endpoint struct {
	install     func(*mcp.Server, *mcp.Tool, []Stage)
	describe    func(*mcp.Tool)
	description Description
}

type Registry[M any] struct {
	server   *mcp.Server
	compiler Compiler[M]
	global   []Stage
	entries  []Entry[M]
	names    map[string]bool
	frozen   bool
}

// New owns a fresh SDK server. Options and protocol capabilities are chosen by
// the application. The compiler is required, even for descriptive-only metadata.
func New[M any](implementation *mcp.Implementation, options *mcp.ServerOptions, compiler Compiler[M], global ...Stage) *Registry[M] {
	if compiler == nil {
		panic("mcpregistry: metadata compiler is required")
	}
	validateStages(global)
	return &Registry[M]{server: mcp.NewServer(implementation, options), compiler: compiler, global: slices.Clone(global), names: map[string]bool{}}
}

func (r *Registry[M]) Register(spec Spec[M], endpoint Endpoint) {
	if r.frozen {
		panic("mcpregistry: registration is frozen")
	}
	if endpoint.install == nil {
		panic("mcpregistry: missing endpoint")
	}
	if !validName(spec.Tool.Name) {
		panic("mcpregistry: invalid tool name " + spec.Tool.Name)
	}
	if r.names[spec.Tool.Name] {
		panic("mcpregistry: duplicate tool " + spec.Tool.Name)
	}
	if endpoint.describe == nil && spec.Tool.InputSchema == nil {
		panic("mcpregistry: " + spec.Tool.Name + ": raw and custom endpoints require an explicit input schema")
	}
	// Capture JSON-valued SDK metadata and schemas, including extension fields.
	// Callers cannot later mutate the served catalog through their declaration.
	spec.Tool = cloneTool(spec.Tool)
	compilerSpec := spec
	compilerSpec.Tool = cloneTool(spec.Tool)
	stages, err := r.compiler(compilerSpec)
	if err != nil {
		panic(fmt.Sprintf("mcpregistry: %s: %v", spec.Tool.Name, err))
	}
	validateStages(stages)
	stages = append(slices.Clone(r.global), stages...)
	served := cloneTool(spec.Tool)
	// Typed endpoints preserve SDK inference and nil-output handling. Infer an
	// independent descriptive copy; wire-parity tests guard this SDK boundary.
	if endpoint.describe != nil {
		endpoint.describe(&spec.Tool)
	}
	install(endpoint, r.server, &served, stages)
	names := make([]string, 0, len(stages))
	for _, stage := range stages {
		names = append(names, stage.Name)
	}
	r.names[spec.Tool.Name] = true
	r.entries = append(r.entries, Entry[M]{Spec: spec, Description: endpoint.description, Stages: names})
}

// Server freezes registration and returns the same SDK server on every call.
// Do not add/remove tools directly on it: doing so bypasses the inventory.
// Transport construction, HTTP middleware, resources, and prompts remain SDK APIs.
func (r *Registry[M]) Server() *mcp.Server { r.frozen = true; return r.server }

// Inventory returns a sorted snapshot, without opening stores or invoking tools.
// Library-owned schemas and descriptions are copied. Application metadata is
// copied by value; referenced data in M must be treated as immutable.
func (r *Registry[M]) Inventory() Inventory[M] {
	entries := slices.Clone(r.entries)
	for i := range entries {
		e := &entries[i]
		e.Tool = cloneTool(e.Tool)
		e.Stages = slices.Clone(e.Stages)
		e.Gaps = slices.Clone(e.Gaps)
		if e.Binding != nil {
			b := *e.Binding
			e.Binding = &b
		}
	}
	slices.SortFunc(entries, func(a, b Entry[M]) int { return strings.Compare(a.Tool.Name, b.Tool.Name) })
	return Inventory[M]{SchemaVersion: 1, Tools: entries}
}

// Report SDK declaration panics (such as invalid schemas) as registry errors.
func install(endpoint Endpoint, server *mcp.Server, tool *mcp.Tool, stages []Stage) {
	defer func() {
		if p := recover(); p != nil {
			panic(fmt.Sprintf("mcpregistry: %v", p))
		}
	}()
	endpoint.install(server, tool, stages)
}

// errInternal reports call-time wiring bugs, such as a nil context from a stage
// or a nil mapped error. The SDK runs handlers in their own goroutines without
// recovering panics, so panicking here would stop the whole server.
var errInternal = errors.New("internal error")

func prepare(ctx context.Context, req *mcp.CallToolRequest, stages []Stage) (context.Context, error) {
	for _, stage := range stages {
		var err error
		ctx, err = stage.Prepare(ctx, req)
		if err != nil {
			return nil, err
		}
		if ctx == nil {
			return nil, errInternal
		}
	}
	return ctx, nil
}

func validateStages(stages []Stage) {
	for _, stage := range stages {
		if stage.Name == "" || stage.Prepare == nil {
			panic("mcpregistry: stage name and preparation function are required")
		}
	}
}

func validName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-.", c)) {
			return false
		}
	}
	return true
}
