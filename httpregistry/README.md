# HTTP registry

`httpregistry` is an application-independent library for registering HTTP
endpoints and inspecting the same declarations that serve requests. It is part
of Manifest and imports neither application code nor the `action` package. It can be used without an application host or action catalog.

## Reading the source

- `registry.go` owns registration, metadata compilation, routing, and inventory.
- `binding.go` defines the public binding types. `Bind` assembles the endpoint;
  `operationHandler` decodes parameters, invokes the typed operation, and writes
  its response. These are the only binding functions with generic type parameters.
- `binding_plan.go` validates declarations once during construction and resolves
  field names into an immutable `bindingPlan`. Each `inputMapping` keeps its input
  declaration beside its destination field index. The plan also describes the
  temporary JSON body struct and how to copy its fields into action parameters.
- `binding_decode.go` uses that plan for each request: decode path/query values,
  decode the body, and fill a fresh parameter struct. It neither invokes actions
  nor chooses error responses.

## Registration and metadata

Create a `Registry[M]` with an application-owned metadata compiler. Register an
endpoint using `Handle(Spec[M], Endpoint)` or an opaque protocol handler using
`Mount(Spec[M], Endpoint)`. Use `Custom(http.Handler)` for ordinary handlers and
`Bind` for typed operations. `CustomFactory(func() http.Handler)` defers custom
handler construction until registration determines that the endpoint is enabled;
disabled declarations retain their description without running the factory.
`Handler()` freezes registration and returns an
`http.Handler`; there is no exposed router or alternative registration path.
Construction is single-threaded. After freezing, requests and inventory reads
may run concurrently. Treat metadata and captured configuration as immutable;
metadata is copied by value, not recursively cloned.

The compiler is mandatory and runs for every declaration, including disabled
ones. It returns named `Stage` values coupling actual wrappers with their
inspectable identities. The registry assigns no policy meaning to metadata.
A descriptive-only compiler can explicitly return no stages. Named global
stages are optional and also run for unmatched paths and unsupported methods.
The first stage is outermost. There is no subrouter policy inheritance.

Invalid registrations and binding plans panic at construction, like the action
catalog's invalid definitions. Duplicate method/pattern shapes, overlapping
mount boundaries, invalid stages, and late registration are rejected. A named
`Condition` controls both installation and the inventory's `enabled` field.
Disabled declarations remain in the inventory, with their prospective stages.

Requests that match no enabled endpoint get the router's plain 404, or 405
with an `Allow` header, unless `SetUnmatched(Unmatched{Name, Handle})` names
an application handler, once and before `Handler()`. It receives the methods
enabled at the request's path: none for an unknown path, otherwise the method
is unsupported. Methods the router does not know, such as `PROPFIND`, are
refused before routing, even under a mount, so they reach this handler too.
Requests for disabled endpoints are unmatched. Global stages wrap the handler;
endpoint stages do not. The inventory names it and its stages.

Chi is currently a private routing implementation. Only literal segments,
whole-segment `{name}` placeholders, and a final `{name...}` placeholder are
accepted. `{name...}` matches the rest of the path, slashes included, and may
be empty; literal and single-segment routes take precedence over it. Its value
is read, and bound, like any other placeholder. Methods are explicit: GET does
not implicitly register HEAD or OPTIONS. Paths retain existing Chi matching and
escaping behavior, without automatic trailing-slash redirects. `PathParam`
reads matched values without exposing Chi. `MatchedPath` names the declared
path of the endpoint or mount that served a request, so a global stage can log
by route once the next handler returns; it is empty for unmatched requests.
Mounts accept a literal path without a trailing slash, delegate that path, its
slash form, and descendants, and do not strip `URL.Path`. Root mounts and
overlapping endpoint/mount boundaries, including a `{name...}` endpoint above a
mount, are not supported in this first version. Router replacement should
preserve or explicitly revise this contract.

## Typed bindings

`Bind(operation, dependencies, Binding{...})` accepts a structural operation
interface with `Name`, `Description`, and `Call(context.Context, D, P) (R, error)`.
The operation must invoke its normal action pipeline. An application-owned
wrapper can satisfy that interface while supplying its dispatcher. The library
neither bypasses that wrapper nor serializes parameters through dynamic JSON
invocation.

A binding declares:

- Each path, query, or JSON-body source and its exported parameter field:
  top-level, or promoted from an embedded struct.
- Fields intentionally left at zero via `Zero`; every field must be accounted for.
- JSON decoding policy, optional byte limit, and unused-query/body behavior.
- Success status, an optional named result encoder, and an
  application-supplied named error writer.

`FieldInputs[P](source)` derives inputs for every field of `P` from one
source, named as `encoding/json` names them (the tag's name, else the Go
field name), and returns `-`-tagged fields as `Zero`. It suits operations
that take one flat parameter document, such as an action catalog served one
route per action. The derived inputs are ordinary declarations: `Bind`
validates them and the inventory lists them.

Path/query mappings support strings, booleans, integers, and floats, types
implementing `encoding.TextUnmarshaler` (such as `uuid.UUID` and `time.Time`),
optional pointers, and query lists. A type's own `UnmarshalText` takes
precedence over conversion by kind. Repeated scalar query values fail even when
equal. Missing optional query values leave the field at zero; `Required`
distinguishes missing values from present empty strings. Other conversion uses
Go's `strconv` functions. Path values come from the router and are not decoded a
second time. Defaults and arbitrary field conversion functions are not supported
yet.

Parameter structs cannot have custom JSON unmarshalling. Struct values may be
embedded; their promoted fields are mapped like top-level ones and must be
accounted for. Embedded pointers, non-struct embeddings, and embedded types with
custom JSON unmarshalling are rejected. Fields Go cannot promote (ambiguous or
shadowed) are left at zero, as JSON ignores them too.
Body names use ASCII letters, digits, underscores, dots, and hyphens (except
`-` alone). Body fields retain their own JSON types and codecs; arbitrary custom field
codecs are not analyzed. Multiple assignments to one parameter field, duplicate
source names, unsupported types, and missing path placeholders fail construction.
There is no path/body merging or precedence mechanism.

`StrictJSON` uses `encoding/json/v2` and requires one object, case-sensitive
field names, no unknown fields, no duplicate object keys (including nested
objects), valid Unicode, and no trailing value. A body-supplied path ID is
therefore rejected. Top-level `null` is rejected; omitted or null body fields
retain JSON v2's normal decoding behavior and operation validation still applies.
`NoJSON` reads no body unless unexpected-body rejection is requested.

A positive `MaxBodyBytes` bounds the complete JSON request body before invocation,
including trailing whitespace. Zero means no binding-level limit, not proof
that no limit exists elsewhere. No content-type
requirement is introduced. Unknown query parameters and unexpected read-route
bodies are ignored by default.

Decode failures arrive at the error writer as `InputError`, preserving wrapped
errors such as `http.MaxBytesError`. Operation errors pass through unchanged.
Successful results are encoded completely with `encoding/json/v2`, or the
binding's named `Encoder` when one is set (for example, to keep an existing
API's `encoding/json` response shapes), and the configured 2xx status;
204/205 and projections are not supported. Encoding happens before committing
headers. Login cookies, response filtering, OAuth forms, streams, and protocol
errors belong in custom handlers.

## Evidence and boundaries

`Inventory()` returns a versioned snapshot sorted by path and method. Each entry
contains full metadata, activation, effective ordered stages, target kind,
executable binding details where available, and explicit inspection gaps.
When an unmatched handler is set, `unmatched` gives its name and stages.

The registry establishes installed dispatch and wrapper construction. A binding
establishes parameter mapping and its direct operation handle. Wrapper names,
error-policy names, and metadata are not proofs that arbitrary application code
implements their stated semantics. Behavioral tests supply that evidence.
Custom handlers and mounts do not claim a complete internal call graph. No direct
binding does **not** mean a request cannot invoke actions through middleware,
custom code, or a protocol server.

This inventory does not establish deployment/reverse-proxy exposure, inspect
transitive action calls, or supply source declaration locations.
Application-specific source inventories remain separate; a binding's
namespace/action pair can be joined to them by tooling. No runtime action
function inspection is needed.

The tests cover metadata enforcement before decoding, text-decoded values,
embedded fields, derived inputs, custom encoders, ordering, routing and
mount boundaries, trailing placeholders, matched paths, unmatched requests,
independent instances, frozen registration, strict versus legacy decoding, conflicting mappings, size
limits, typed invocation, and snapshot isolation. An architecture check rejects dependencies on application
code and other Manifest packages.
