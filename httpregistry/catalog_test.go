package httpregistry_test

import (
	"github.com/jackc/manifest/action"
	"github.com/jackc/manifest/httpregistry"
)

// Actions defined in an action.Catalog bind directly, with no application
// wrapper supplying the dispatcher.
var _ httpregistry.Operation[*struct{}, struct{}, string] = (*action.CatalogAction[*struct{}, struct{}, struct{}, string])(nil)
