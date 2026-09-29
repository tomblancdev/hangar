// Package api carries the published contract: the OpenAPI 3.1 document every
// client is written against (the command line and the console included).
package api

import _ "embed"

// OpenAPI is the contract, as YAML; the brain serves it as JSON.
//
//go:embed openapi.yaml
var OpenAPI []byte
