# MCP registry

`mcpregistry` couples explicit MCP tool exposure with execution and an inventory.
It uses the official Go MCP SDK; it does not implement the wire protocol or an
OAuth server. It imports neither other Manifest packages nor application code.

## Registration and lifetime

Create a `Registry[M]` with the application's implementation identity, SDK server
options, and a metadata compiler. Register each tool with a `Spec[M]` and an
endpoint from `Bind`, `BindRaw`, `Typed`, or `Custom`. The compiler must be
explicit even if metadata is descriptive only (return no stages).

Names are explicit, case-sensitive, unique in the registry, and limited to
1–128 ASCII letters, digits, underscores, hyphens, and periods. Registering an
action elsewhere never exposes it as a tool. Invalid declarations panic during
construction, as in Manifest's other registries. This includes the SDK's own tool
checks and `BindRaw` or `Custom` endpoints without an explicit input schema.
Register before serving; construction is single-threaded. `Server()` freezes registration and returns the
same SDK server on repeated calls. Use it with any SDK transport. Do not add or
remove tools directly on that server: this bypasses the inventory.

The application owns `mcp.ServerOptions`, capabilities, transport configuration,
and whether its HTTP endpoint is enabled. The registry contains the tools that
would be served when that endpoint is enabled. It has no OAuth, database,
filesystem, HTTP routing, or background-job lifecycle. Resources and prompts
remain SDK APIs and are outside this tool inventory.

## Endpoint choices

| Constructor | Arguments and results | Inventory evidence |
| --- | --- | --- |
| `Bind(operation, dependencies, binding)` | SDK typed inference, decoding, validation, and results; invokes `operation.Call` | Direct operation identity from the same handle that executes |
| `BindRaw(operation, dependencies, binding)` | Original JSON argument bytes passed to `operation.InvokeJSON`; explicit schemas; optional byte cap and result formatter | Direct operation identity, argument cap, named error/result policies |
| `Typed(handler)` | SDK typed handler, including custom input/output types and content | Custom handler; no inferred action edges |
| `Custom(handler)` | SDK raw handler with full request/result control | Custom handler; schema enforcement and internal calls are opaque |

Typed operations have this structural interface:

```go
type Operation[D, P, R any] interface {
    Name() string
    Description() string
    Call(context.Context, D, P) (R, error)
}
```

This accepts application action wrappers without depending on `manifest/action`.
Applications using `action.Action` directly can adapt their dispatcher into `D`
or provide a small wrapper, as they do for HTTP bindings. Invocation must retain
the application's validation, authorization, and audit pipeline.

Raw operations similarly provide `Name`, `Description`, and
`InvokeJSON(context.Context, D, []byte) (any, error)`. An application adapter can
delegate that method to its own dispatcher using the action handle's name.
The registry never round-trips the arguments through `map[string]any`, inserts
defaults, or validates them against the advertised schema. This preserves number
tokens and extension fields and allows an import to return individual row errors.
The operation must perform its own decoding and validation. A schema in the tool
catalog is not evidence that raw calls enforce it. The byte cap measures the
argument document, not the whole MCP/HTTP request; HTTP limits remain separate.

`BindRaw` defaults to JSON structured content and the same JSON as text. A null
result returns neither, as the SDK does for nil typed output. An application can install a named formatter to produce conversational text beside
structured results. It owns output schema validation. For more complex behavior,
use a custom handler; for example, replaying a prior write before fetching files
must keep that ordering in application code.

## Policies and trust boundaries

A metadata compiler supplies named `Stage` values. Global stages run first,
followed by compiled stages in order. Each prepares the context or returns a
client-safe error, stopping the call. For every endpoint kind, a stage's
`*jsonrpc.Error` is a protocol error and other errors are `IsError` results.
Context identity is resolved per call, never retained in registry state. Typed
stages run after SDK argument validation and decoding, before the application
handler. Raw stages run before the raw handler and its argument cap. These are not substitutes for authentication of HTTP discovery and
other protocol methods: protect the transport separately.

Bindings require a named `ErrorPolicy` to map operation failures to safe client
errors. Raw result-formatting failures also pass through that policy. SDK input
errors and stage errors do not. Custom handlers own their own error handling,
including intentionally returning protocol errors. Normal typed handler errors
use the SDK's tool-error handling; raw binding errors produce `IsError` results.

Wiring bugs detected during a call, such as an error policy returning nil or a
stage returning a nil context, produce an `internal error` tool result rather
than a panic. The SDK runs handlers in their own goroutines without recovering
panics, so one would stop the whole server.

`Spec.Tool` preserves SDK tool fields, including explicit schemas, descriptions,
annotations, and extension `_meta`. The library does not derive annotations from
action mutation flags, install retries, fetch files, or interpret idempotency
keys. These have application-defined semantics.

## Inventory contract

`Inventory()` returns schema version 1 and tools sorted by name, from the same
registrations that install executable handlers. Each entry contains the complete
tool definition, application metadata, installed stage names, endpoint kind,
argument-processing mode, optional action binding, and explicit inspection gaps.
Constructors and compilers must be free of I/O so inventories can be built with
non-serving dependencies, without invoking actions or opening services.

Library-owned definitions are snapshots: schemas, annotations, extension data,
stage lists, and binding descriptions cannot mutate the serving registry.
Application metadata `M` is copied by value; callers must treat referenced data
as immutable. Tool metadata must be JSON-serializable, as required on the wire.

The SDK infers typed schemas into a private copy. The registry independently
infers descriptive schemas using the same `jsonschema-go` rules while leaving
the actual typed registration to the SDK. This preserves its pointer-result and
default-handling behavior. Tests compare inventories with `tools/list` for
inferred, explicit, pointer, `any`, and non-object output schemas. They should be
run when updating the SDK. SDK-owned metadata and raw result content use the
SDK's `encoding/json` serialization semantics; application argument decoders are
independent.

Inventory evidence establishes which direct operation handle or custom handler
is installed, and which named policies surround it. It does not prove policy
behavior, read-only/idempotency annotations, custom codec semantics, or transitive
action calls. Custom formatters can themselves call actions; those calls are also
outside the described direct binding.
