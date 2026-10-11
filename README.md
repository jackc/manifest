# Manifest

Manifest provides Go building blocks for legible, verifiable backend applications.
Its declarations drive execution and expose application structure for inspection.
Applications own their dependencies, metadata, and policy semantics.

## Packages

- `github.com/jackc/manifest/action`: typed action catalogs, middleware dispatch,
  parameter validation, and dynamic JSON invocation. Registries and dispatchers
  are explicit instances; dependencies are supplied per invocation.
- `github.com/jackc/manifest/httpregistry`: HTTP routing, metadata compilation,
  typed operation bindings, and inventories derived from the same declarations
  that serve requests. See the [HTTP registry contract](httpregistry/README.md).

The packages are independent. HTTP bindings accept a structural operation
interface, so applications can adapt their action pipeline without coupling the
HTTP registry to a particular catalog or application host.

Inspectable declarations establish wiring and parameter mappings. Behavioral
tests provide evidence that application handlers and policies implement their
stated semantics; inventory metadata alone does not prove those semantics.

## Requirements and installation

Go 1.27.2 or later is required. Manifest uses generic methods and
`encoding/json/v2`.

```sh
go get github.com/jackc/manifest
```

## Development

```sh
go test -race ./...
go vet ./...
```

Manifest was extracted from [Logger4Life](https://github.com/jackc/logger4life).
The initial API is evolving.
