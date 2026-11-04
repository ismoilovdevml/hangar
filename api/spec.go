// Package api embeds the OpenAPI document of the REST API, api/openapi.yaml,
// so the server can publish the contract it implements.
package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"

	"go.yaml.in/yaml/v3"
)

// OpenAPIYAML is api/openapi.yaml as built into the binary.
//
//go:embed openapi.yaml
var OpenAPIYAML []byte

var (
	jsonOnce sync.Once
	jsonDoc  []byte
	errJSON  error
)

// OpenAPIJSON returns the OpenAPI document as JSON, converted once.
func OpenAPIJSON() ([]byte, error) {
	jsonOnce.Do(func() {
		var doc any
		if err := yaml.Unmarshal(OpenAPIYAML, &doc); err != nil {
			errJSON = fmt.Errorf("parse openapi.yaml: %w", err)
			return
		}
		jsonDoc, errJSON = json.Marshal(doc)
		if errJSON != nil {
			errJSON = fmt.Errorf("encode openapi.json: %w", errJSON)
		}
	})
	return jsonDoc, errJSON
}
