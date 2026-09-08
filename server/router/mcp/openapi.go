package mcp

import (
	"os"
	"strings"

	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

type jsonSchema map[string]any

type openAPISpec struct {
	OpenAPI    string                                  `yaml:"openapi"`
	Paths      map[string]map[string]*openAPIOperation `yaml:"paths"`
	Components openAPIComponents                       `yaml:"components"`
}

type openAPIComponents struct {
	Schemas map[string]jsonSchema `yaml:"schemas"`
}

type openAPIOperation struct {
	OperationID       string                     `yaml:"operationId"`
	Description       string                     `yaml:"description"`
	Parameters        []openAPIParameter         `yaml:"parameters"`
	RequestBody       *openAPIRequestBody        `yaml:"requestBody"`
	Responses         map[string]openAPIResponse `yaml:"responses"`
	Method            string                     `yaml:"-"`
	Path              string                     `yaml:"-"`
	ResponseSchema    jsonSchema                 `yaml:"-"`
	RequestBodySchema jsonSchema                 `yaml:"-"`
}

type openAPIParameter struct {
	Name        string     `yaml:"name"`
	In          string     `yaml:"in"`
	Description string     `yaml:"description"`
	Required    bool       `yaml:"required"`
	Schema      jsonSchema `yaml:"schema"`
}

type openAPIRequestBody struct {
	Required bool                        `yaml:"required"`
	Content  map[string]openAPIMediaType `yaml:"content"`
}

type openAPIResponse struct {
	Description string                      `yaml:"description"`
	Content     map[string]openAPIMediaType `yaml:"content"`
}

type openAPIMediaType struct {
	Schema jsonSchema `yaml:"schema"`
}

func loadOpenAPISpec(path string) (*openAPISpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "failed to read OpenAPI spec")
	}

	spec := &openAPISpec{}
	if err := yaml.Unmarshal(data, spec); err != nil {
		return nil, errors.Wrap(err, "failed to parse OpenAPI spec")
	}
	if spec.Paths == nil {
		return nil, errors.New("OpenAPI spec has no paths")
	}
	return spec, nil
}

func buildOperationRegistry(spec *openAPISpec) (map[string]*openAPIOperation, error) {
	registry := map[string]*openAPIOperation{}
	for path, pathItem := range spec.Paths {
		for method, operation := range pathItem {
			if operation == nil || operation.OperationID == "" {
				continue
			}
			if _, exists := registry[operation.OperationID]; exists {
				return nil, errors.Errorf("duplicate OpenAPI operationId %q", operation.OperationID)
			}

			operation.Method = strings.ToUpper(method)
			operation.Path = path

			responseSchema, err := operationSuccessResponseSchema(spec, operation)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to resolve response schema for %s", operation.OperationID)
			}
			operation.ResponseSchema = responseSchema

			requestBodySchema, err := operationRequestBodySchema(spec, operation)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to resolve request body schema for %s", operation.OperationID)
			}
			operation.RequestBodySchema = requestBodySchema

			registry[operation.OperationID] = operation
		}
	}
	return registry, nil
}

func operationSuccessResponseSchema(spec *openAPISpec, operation *openAPIOperation) (jsonSchema, error) {
	response, ok := operation.Responses["200"]
	if !ok || response.Content == nil {
		return okSchema(), nil
	}
	mediaType, ok := response.Content["application/json"]
	if !ok || mediaType.Schema == nil {
		return okSchema(), nil
	}
	return resolveSchemaRef(spec, mediaType.Schema)
}

func operationRequestBodySchema(spec *openAPISpec, operation *openAPIOperation) (jsonSchema, error) {
	if operation.RequestBody == nil {
		return nil, nil
	}
	mediaType, ok := operation.RequestBody.Content["application/json"]
	if !ok || mediaType.Schema == nil {
		return jsonSchema{"type": "object"}, nil
	}
	return resolveSchemaRef(spec, mediaType.Schema)
}

func resolveSchemaRef(spec *openAPISpec, schema jsonSchema) (jsonSchema, error) {
	resolved, err := inlineSchemaValue(spec, schema, map[string]bool{})
	if err != nil {
		return nil, err
	}

	resolvedSchema, ok := resolved.(map[string]any)
	if !ok {
		return nil, errors.New("resolved schema is not an object")
	}
	return jsonSchema(resolvedSchema), nil
}

func inlineSchemaValue(spec *openAPISpec, value any, resolving map[string]bool) (any, error) {
	switch typed := value.(type) {
	case jsonSchema:
		return inlineSchemaMap(spec, map[string]any(typed), resolving)
	case map[string]any:
		return inlineSchemaMap(spec, typed, resolving)
	case []any:
		inlined := make([]any, 0, len(typed))
		for _, item := range typed {
			inlinedItem, err := inlineSchemaValue(spec, item, resolving)
			if err != nil {
				return nil, err
			}
			inlined = append(inlined, inlinedItem)
		}
		return inlined, nil
	default:
		return value, nil
	}
}

func inlineSchemaMap(spec *openAPISpec, schema map[string]any, resolving map[string]bool) (map[string]any, error) {
	if ref, ok := schema["$ref"].(string); ok && ref != "" {
		name, err := schemaComponentName(ref)
		if err != nil {
			return nil, err
		}
		return inlineComponentSchema(spec, name, resolving)
	}

	inlined := make(map[string]any, len(schema))
	for key, value := range schema {
		inlinedValue, err := inlineSchemaValue(spec, value, resolving)
		if err != nil {
			return nil, err
		}
		inlined[key] = inlinedValue
	}
	normalizeSchemaFormat(inlined)
	return inlined, nil
}

// inlineComponentSchema expands a component schema in place so MCP tool
// schemas carry no $ref pointers for consumers to resolve. Every nested
// component reference is expanded recursively; the resolving set guards
// against circular schemas, which cannot be fully inlined.
func inlineComponentSchema(spec *openAPISpec, name string, resolving map[string]bool) (map[string]any, error) {
	if resolving[name] {
		return nil, errors.Errorf("circular schema ref %q", schemaComponentRef(name))
	}
	component, ok := spec.Components.Schemas[name]
	if !ok {
		return nil, errors.Errorf("schema ref %q not found", schemaComponentRef(name))
	}
	resolving[name] = true
	inlined, err := inlineSchemaMap(spec, map[string]any(component), resolving)
	delete(resolving, name)
	if err != nil {
		return nil, err
	}
	return inlined, nil
}

// inlineLocalRefs expands any remaining local "#/$defs/<Name>" pointers using
// the schema's own $defs table and drops $defs, so the schema handed to MCP
// consumers is fully self-contained with no $ref left to resolve. Unknown or
// circular pointers are left in place rather than failing schema generation.
func inlineLocalRefs(schema jsonSchema) jsonSchema {
	defs := map[string]any{}
	if rawDefs, ok := schema["$defs"]; ok {
		if typed, ok := rawDefs.(map[string]any); ok {
			defs = typed
		}
	}
	expanded, _ := expandLocalRefValue(map[string]any(schema), defs, map[string]bool{}).(map[string]any)
	if expanded == nil {
		expanded = map[string]any{}
	}
	delete(expanded, "$defs")
	return jsonSchema(expanded)
}

func expandLocalRefValue(value any, defs map[string]any, resolving map[string]bool) any {
	switch typed := value.(type) {
	case jsonSchema:
		return expandLocalRefValue(map[string]any(typed), defs, resolving)
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#/$defs/") {
			name := strings.TrimPrefix(ref, "#/$defs/")
			if resolving[name] {
				return typed
			}
			definition, ok := defs[name]
			if !ok {
				return typed
			}
			resolving[name] = true
			expanded := expandLocalRefValue(definition, defs, resolving)
			delete(resolving, name)
			if len(typed) == 1 {
				return expanded
			}
			merged := map[string]any{}
			if expandedMap, ok := expanded.(map[string]any); ok {
				for key, item := range expandedMap {
					merged[key] = item
				}
			}
			for key, item := range typed {
				if key == "$ref" {
					continue
				}
				merged[key] = expandLocalRefValue(item, defs, resolving)
			}
			return merged
		}
		expanded := make(map[string]any, len(typed))
		for key, item := range typed {
			if key == "$defs" {
				continue
			}
			expanded[key] = expandLocalRefValue(item, defs, resolving)
		}
		return expanded
	case []any:
		expanded := make([]any, 0, len(typed))
		for _, item := range typed {
			expanded = append(expanded, expandLocalRefValue(item, defs, resolving))
		}
		return expanded
	default:
		return value
	}
}

// normalizeSchemaFormat rewrites the OpenAPI-only "format" markers emitted by
// the gnostic generator into standard JSON Schema so MCP clients can consume
// tool schemas without warnings or strict-mode failures.
//
//   - "enum" duplicates the "enum" keyword and is dropped.
//   - "bytes" becomes "contentEncoding: base64", matching the protojson wire encoding.
//   - "field-mask" has no JSON Schema equivalent and is dropped.
func normalizeSchemaFormat(schema map[string]any) {
	format, ok := schema["format"].(string)
	if !ok {
		return
	}
	switch format {
	case "enum", "field-mask":
		delete(schema, "format")
	case "bytes":
		delete(schema, "format")
		schema["contentEncoding"] = "base64"
	}
}

// sanitizeSchemaValue deep-copies a schema value that did not pass through the
// component resolver (such as parameter schemas) and normalizes its formats.
func sanitizeSchemaValue(value any) any {
	switch typed := value.(type) {
	case jsonSchema:
		return sanitizeSchemaValue(map[string]any(typed))
	case map[string]any:
		sanitized := make(map[string]any, len(typed))
		for key, item := range typed {
			sanitized[key] = sanitizeSchemaValue(item)
		}
		normalizeSchemaFormat(sanitized)
		return sanitized
	case []any:
		sanitized := make([]any, 0, len(typed))
		for _, item := range typed {
			sanitized = append(sanitized, sanitizeSchemaValue(item))
		}
		return sanitized
	default:
		return value
	}
}

func schemaComponentName(ref string) (string, error) {
	const prefix = "#/components/schemas/"
	if !strings.HasPrefix(ref, prefix) {
		return "", errors.Errorf("unsupported schema ref %q", ref)
	}
	return strings.TrimPrefix(ref, prefix), nil
}

func schemaComponentRef(name string) string {
	return "#/components/schemas/" + name
}

func okSchema() jsonSchema {
	return jsonSchema{
		"type": "object",
		"properties": map[string]any{
			"ok": map[string]any{"type": "boolean"},
		},
	}
}
