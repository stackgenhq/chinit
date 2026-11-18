// Copyright (C) 2026 StackGen, Inc. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package operator

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// parseSpec unmarshals spec YAML into a CR the same way the Kubernetes API
// server does: yaml → JSON → json.Unmarshal. The operator's Settings type
// implements UnmarshalJSON only (its yaml.v3 unmarshaler is commented out),
// so a direct yaml.Unmarshal into the CR silently yields empty
// users/profiles/settings — a spec that appears to load but renders nothing.
func parseSpec(yamlBytes []byte, cr any) error {
	jsonBytes, err := yamlToJSON(yamlBytes)
	if err != nil {
		return fmt.Errorf("convert spec YAML to JSON: %w", err)
	}
	if err := json.Unmarshal(jsonBytes, cr); err != nil {
		return err
	}
	return nil
}

// yamlToJSON converts a YAML document to JSON.
func yamlToJSON(yamlBytes []byte) ([]byte, error) {
	var node map[string]any
	if err := yaml.Unmarshal(yamlBytes, &node); err != nil {
		return nil, err
	}
	return json.Marshal(node)
}

// joinErrors formats status errors for an error message.
func joinErrors(errors []string) string {
	return strings.Join(errors, "; ")
}
