package mcpregistry_test

import (
	"github.com/jackc/manifest/action"
	"github.com/jackc/manifest/mcpregistry"
)

// Actions defined in an action.Catalog bind directly, typed or raw, with no
// application wrapper supplying the dispatcher.
var (
	_ mcpregistry.Operation[*struct{}, struct{}, string] = (*action.CatalogAction[*struct{}, struct{}, struct{}, string])(nil)
	_ mcpregistry.RawOperation[*struct{}]                = (*action.CatalogAction[*struct{}, struct{}, struct{}, string])(nil)
)
