package openapigen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	path_ "path"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
)

type GroupRegex struct {
	Group string
	Regex string
}

type OpenAPIGen struct {
	doc               *openapi3.T
	registeredEnums   map[reflect.Type][]any
	groupRegexes      []GroupRegex
	customTypeSchemas map[reflect.Type]*openapi3.SchemaRef
}

const DefaultTag = ""

func New(title string, version string) *OpenAPIGen {
	gen := &OpenAPIGen{
		doc: &openapi3.T{
			OpenAPI: "3.0.0",
			Info: &openapi3.Info{
				Title:   title,
				Version: version,
			},
			Paths: &openapi3.Paths{},
			Components: &openapi3.Components{
				Schemas: make(openapi3.Schemas),
			},
		},
		registeredEnums:   make(map[reflect.Type][]any),
		groupRegexes:      make([]GroupRegex, 0),
		customTypeSchemas: make(map[reflect.Type]*openapi3.SchemaRef),
	}

	gen.RegisterCustomType(reflect.TypeOf(time.Time{}), openapi3.NewSchemaRef("", openapi3.NewDateTimeSchema()))

	return gen
}

func (o *OpenAPIGen) AddGroup(group string, regex string) *OpenAPIGen {
	o.groupRegexes = append(o.groupRegexes, GroupRegex{Group: group, Regex: regex})
	return o
}

func (o *OpenAPIGen) walkInputFields(typ reflect.Type, visit func(field reflect.StructField) error) error {
	if typ == nil {
		return nil
	}
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	if typ.Kind() != reflect.Struct {
		return nil
	}

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)

		// unexported alanları atla
		if f.PkgPath != "" {
			continue
		}

		formFileTag := f.Tag.Get("formFile")
		formTag := f.Tag.Get("form")
		paramTag := f.Tag.Get("param")
		queryTag := f.Tag.Get("query")
		jsonTag := f.Tag.Get("json")

		// Inner struct flatten koşulları:
		// 1) embedded (anonymous)
		// 2) wrapper field: json:"-" ve kendisi param/query/form tag taşımıyor (içine in)
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}

		shouldFlatten :=
			(f.Anonymous && ft.Kind() == reflect.Struct) ||
				(ft.Kind() == reflect.Struct &&
					jsonTag == "-" &&
					formFileTag == "" && formTag == "" && paramTag == "" && queryTag == "")

		// time.Time gibi struct'lara dalma
		if shouldFlatten && ft != reflect.TypeOf(time.Time{}) {
			if err := o.walkInputFields(ft, visit); err != nil {
				return err
			}
			continue
		}

		if err := visit(f); err != nil {
			return err
		}
	}

	return nil
}

func (o *OpenAPIGen) AddRoute(path string, method string, inputType reflect.Type, tag string) error {
	operation := &openapi3.Operation{
		Parameters: openapi3.Parameters{},
		Responses:  openapi3.NewResponses(),
	}

	if tag == "" {
		for _, groupRegex := range o.groupRegexes {
			if match, _ := regexp.MatchString(groupRegex.Regex, path); match {
				tag = groupRegex.Group
				break
			}
		}
	}

	if tag != "" {
		operation.Tags = []string{tag}

		foundGlobalTag := false
		for _, existingTag := range o.doc.Tags {
			if existingTag.Name == tag {
				foundGlobalTag = true
				break
			}
		}
		if !foundGlobalTag {
			o.doc.Tags = append(o.doc.Tags, &openapi3.Tag{Name: tag, Description: ""})
		}
	}

	operation.Responses.Set("200", &openapi3.ResponseRef{
		Value: openapi3.NewResponse().WithDescription("OK"),
	})

	if inputType != nil {
		typ := inputType
		if typ.Kind() == reflect.Ptr {
			typ = typ.Elem()
		}

		if typ.Kind() == reflect.Struct {
			_, err := o.typeToSchema(typ)
			if err != nil {
				return fmt.Errorf("failed to generate schema for request body type %s: %w", path_.Base(typ.PkgPath())+"."+typ.Name(), err)
			}

			schemaName := path_.Base(typ.PkgPath()) + "." + typ.Name()
			componentSchemaRef, found := o.doc.Components.Schemas[schemaName]
			if !found || componentSchemaRef == nil || componentSchemaRef.Value == nil {
				return fmt.Errorf("schema definition for %s not found in components, though it should have been generated", schemaName)
			}
			actualSchema := componentSchemaRef.Value

			hasJSONFields := false

			err = o.walkInputFields(typ, func(field reflect.StructField) error {
				formFileTag := field.Tag.Get("formFile")
				formTag := field.Tag.Get("form")
				paramTag := field.Tag.Get("param")
				queryTag := field.Tag.Get("query")
				jsonTag := field.Tag.Get("json")

				// multipart/form-data (formFile / form)
				if (formFileTag != "" && formFileTag != "-") || (formTag != "" && formTag != "-") {
					if operation.RequestBody == nil {
						operation.RequestBody = &openapi3.RequestBodyRef{
							Value: &openapi3.RequestBody{
								Content: openapi3.NewContent(),
							},
						}
					}

					if operation.RequestBody.Value.Content["multipart/form-data"] == nil {
						operation.RequestBody.Value.Content["multipart/form-data"] = &openapi3.MediaType{
							Schema: openapi3.NewObjectSchema().NewRef(),
						}
					}

					existingSchema := operation.RequestBody.Value.Content["multipart/form-data"].Schema.Value

					if formFileTag != "" && formFileTag != "-" {
						fileSchema := openapi3.NewStringSchema().WithFormat("binary")
						descriptionTag := field.Tag.Get("description")
						if descriptionTag != "" {
							fileSchema.Description = descriptionTag
						}
						existingSchema.Properties[formFileTag] = fileSchema.NewRef()
					} else if formTag != "" && formTag != "-" {
						fieldSchema, err := o.typeToSchema(field.Type)
						if err != nil {
							return fmt.Errorf("failed to generate schema for form field %s: %w", formTag, err)
						}
						descriptionTag := field.Tag.Get("description")
						if descriptionTag != "" && fieldSchema.Value != nil {
							fieldSchema.Value.Description = descriptionTag
						}
						existingSchema.Properties[formTag] = fieldSchema
					}
					return nil
				}

				// path param
				if paramTag != "" && paramTag != "-" {
					paramName := paramTag
					fieldSchemaRef, err := o.typeToSchema(field.Type)
					if err != nil {
						return fmt.Errorf("failed to generate schema for path parameter %s: %w", paramName, err)
					}
					parameter := openapi3.NewPathParameter(paramName).WithSchema(fieldSchemaRef.Value)

					descriptionTag := field.Tag.Get("description")
					if descriptionTag != "" {
						parameter.Description = descriptionTag
					}
					exampleTag := field.Tag.Get("example")
					if exampleTag != "" && fieldSchemaRef.Value != nil {
						var schemaTypeStr string
						if fieldSchemaRef.Value.Type != nil && len(*fieldSchemaRef.Value.Type) > 0 {
							schemaTypeStr = (*fieldSchemaRef.Value.Type)[0]
						}
						parsedExample, _ := parseExample(exampleTag, schemaTypeStr)
						parameter.Example = parsedExample
					}

					operation.Parameters = append(operation.Parameters, &openapi3.ParameterRef{Value: parameter})
					return nil
				}

				// query param
				if queryTag != "" && queryTag != "-" {
					queryName := queryTag
					fieldSchemaRef, err := o.typeToSchema(field.Type)
					if err != nil {
						return fmt.Errorf("failed to generate schema for query parameter %s: %w", queryName, err)
					}
					parameter := openapi3.NewQueryParameter(queryName).WithSchema(fieldSchemaRef.Value)

					descriptionTag := field.Tag.Get("description")
					if descriptionTag != "" {
						parameter.Description = descriptionTag
					}
					exampleTag := field.Tag.Get("example")
					if exampleTag != "" && fieldSchemaRef.Value != nil {
						var schemaTypeStr string
						if fieldSchemaRef.Value.Type != nil && len(*fieldSchemaRef.Value.Type) > 0 {
							schemaTypeStr = (*fieldSchemaRef.Value.Type)[0]
						}
						parsedExample, _ := parseExample(exampleTag, schemaTypeStr)
						parameter.Example = parsedExample
					}

					operation.Parameters = append(operation.Parameters, &openapi3.ParameterRef{Value: parameter})
					return nil
				}

				// json body var mı?
				if jsonTag != "" && jsonTag != "-" {
					hasJSONFields = true
				}

				return nil
			})

			if err != nil {
				return err
			}

			if hasJSONFields {
				operation.RequestBody = &openapi3.RequestBodyRef{
					Value: openapi3.NewRequestBody().WithContent(openapi3.NewContentWithJSONSchema(actualSchema)),
				}
			}
		} else {
			return errors.New("input type for request body must be a struct or pointer to a struct")
		}
	}

	openAPIPath := path
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ":") {
			parts[i] = "{" + strings.TrimPrefix(part, ":") + "}"
		}
	}
	openAPIPath = strings.Join(parts, "/")

	pathItem := o.doc.Paths.Value(openAPIPath)
	if pathItem == nil {
		pathItem = &openapi3.PathItem{}
		o.doc.Paths.Set(openAPIPath, pathItem)
	}

	switch strings.ToUpper(method) {
	case "GET":
		pathItem.Get = operation
	case "POST":
		pathItem.Post = operation
	case "PUT":
		pathItem.Put = operation
	case "DELETE":
		pathItem.Delete = operation
	case "PATCH":
		pathItem.Patch = operation
	default:
		return fmt.Errorf("unsupported HTTP method: %s", method)
	}

	// log.Printf("Registered route: %s %s %s\n", method, openAPIPath, inputType.Name())
	return nil
}

func (o *OpenAPIGen) typeToSchema(typ reflect.Type) (*openapi3.SchemaRef, error) {
	originalTyp := typ
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}

	// Check for custom type schema first
	if customSchema, ok := o.customTypeSchemas[typ]; ok {
		return customSchema, nil
	}
	if customSchema, ok := o.customTypeSchemas[originalTyp]; ok {
		return customSchema, nil
	}

	switch typ.Kind() {
	case reflect.String:
		schemaToReturn := openapi3.NewStringSchema()
		if enumValues, isRegisteredEnum := o.registeredEnums[originalTyp]; isRegisteredEnum {
			schemaToReturn.Enum = enumValues
		} else if enumValues, isRegisteredEnum := o.registeredEnums[typ]; isRegisteredEnum {
			schemaToReturn.Enum = enumValues
		}
		return schemaToReturn.NewRef(), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		schemaToReturn := openapi3.NewIntegerSchema()
		if enumValues, isRegisteredEnum := o.registeredEnums[originalTyp]; isRegisteredEnum {
			schemaToReturn.Enum = enumValues
		} else if enumValues, isRegisteredEnum := o.registeredEnums[typ]; isRegisteredEnum {
			schemaToReturn.Enum = enumValues
		}
		return schemaToReturn.NewRef(), nil
	case reflect.Float32, reflect.Float64:
		schemaToReturn := openapi3.NewFloat64Schema()
		if enumValues, isRegisteredEnum := o.registeredEnums[originalTyp]; isRegisteredEnum {
			schemaToReturn.Enum = enumValues
		} else if enumValues, isRegisteredEnum := o.registeredEnums[typ]; isRegisteredEnum {
			schemaToReturn.Enum = enumValues
		}
		return schemaToReturn.NewRef(), nil
	case reflect.Bool:
		return openapi3.NewBoolSchema().NewRef(), nil
	case reflect.Slice:
		elemSchema, err := o.typeToSchema(typ.Elem())
		if err != nil {
			return nil, err
		}
		return openapi3.NewArraySchema().WithItems(elemSchema.Value).NewRef(), nil
	case reflect.Ptr:
		return o.typeToSchema(typ.Elem())
	case reflect.Struct:
		schemaName := path_.Base(typ.PkgPath()) + "." + typ.Name()
		if schemaName == "." {
			return nil, errors.New("anonymous structs are not supported for schema generation")
		}
		if existingRef, ok := o.doc.Components.Schemas[schemaName]; ok {
			return existingRef, nil
		}

		objSchema := openapi3.NewObjectSchema()
		objSchema.Properties = make(map[string]*openapi3.SchemaRef)

		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			formFileTag := field.Tag.Get("formFile")
			if formFileTag != "" && formFileTag != "-" {
				continue
			}

			jsonTag := field.Tag.Get("json")
			fieldName := field.Name
			if jsonTag == "-" {
				continue
			}
			nameInSchema := fieldName
			if jsonTag != "" {
				parts := strings.Split(jsonTag, ",")
				nameInSchema = parts[0]
			}

			fieldSchemaRef, err := o.typeToSchema(field.Type)
			if err != nil {
				return nil, fmt.Errorf("error generating schema for field %s: %w", fieldName, err)
			}

			if fieldSchemaRef.Value != nil {
				var schemaTypeStr string
				currentFieldType := field.Type
				if currentFieldType.Kind() == reflect.Ptr {
					currentFieldType = currentFieldType.Elem()
				}

				isFieldRegisteredEnum := false
				if enumValues, isRegisteredEnum := o.registeredEnums[currentFieldType]; isRegisteredEnum {
					fieldSchemaRef.Value.Enum = enumValues
					isFieldRegisteredEnum = true
				}

				if fieldSchemaRef.Value.Type != nil && len(*fieldSchemaRef.Value.Type) > 0 {
					schemaTypeStr = (*fieldSchemaRef.Value.Type)[0]
				}

				descriptionTag := field.Tag.Get("description")
				if descriptionTag != "" {
					fieldSchemaRef.Value.Description = descriptionTag
				}

				if !isFieldRegisteredEnum {
					exampleTag := field.Tag.Get("example")
					if exampleTag != "" {
						parsedExample, _ := parseExample(exampleTag, schemaTypeStr)
						fieldSchemaRef.Value.Example = parsedExample
					}
				}
			}
			objSchema.Properties[nameInSchema] = fieldSchemaRef
		}

		schemaRef := openapi3.NewSchemaRef("", objSchema)
		o.doc.Components.Schemas[schemaName] = schemaRef
		return schemaRef, nil

	default:
		return nil, fmt.Errorf("unsupported type for schema generation: %s", typ.Kind())
	}
}

func parseExample(exampleStr string, schemaTypeStr string) (any, error) {
	switch schemaTypeStr {
	case "string":
		return exampleStr, nil
	case "integer":
		val, err := strconv.ParseInt(exampleStr, 10, 64)
		if err != nil {
			return exampleStr, err
		}
		return int(val), nil
	case "number":
		val, err := strconv.ParseFloat(exampleStr, 64)
		if err != nil {
			return exampleStr, err
		}
		return val, nil
	case "boolean":
		val, err := strconv.ParseBool(exampleStr)
		if err != nil {
			return exampleStr, err
		}
		return val, nil
	default:
		return exampleStr, nil
	}
}

func (o *OpenAPIGen) Register(data map[string]any) error {
	path, ok2 := data["path"].(string)
	method, ok1 := data["method"].(string)
	inputType, ok3 := data["inputType"].(reflect.Type)
	tag, _ := data["tag"].(string)

	if !ok1 || !ok2 || !ok3 {
		return errors.New("invalid map")
	}

	return o.AddRoute(path, method, inputType, tag)
}

func (o *OpenAPIGen) RegisterEnum(enumType reflect.Type, values []any) error {
	if enumType == nil {
		return errors.New("enumType cannot be nil")
	}
	if len(values) == 0 {
		return errors.New("enum values cannot be nil or empty")
	}

	actualType := enumType
	if actualType.Kind() == reflect.Ptr {
		actualType = actualType.Elem()
	}

	o.registeredEnums[actualType] = values
	// log.Printf("Registered enum for type %s with %d values", actualType.String(), len(values))
	return nil
}

func (o *OpenAPIGen) RegisterType(typ reflect.Type) error {
	if typ == nil {
		return errors.New("type cannot be nil")
	}

	_, err := o.typeToSchema(typ)
	if err != nil {
		typeName := typ.String()
		return fmt.Errorf("failed to register type %s: %w", typeName, err)
	}
	// log.Printf("Registered type %s", typ.String())
	return nil
}

func (o *OpenAPIGen) RegisterCustomType(typ reflect.Type, schema *openapi3.SchemaRef) error {
	if typ == nil || schema == nil {
		return errors.New("type and schema cannot be nil")
	}
	actualType := typ
	if actualType.Kind() == reflect.Ptr {
		actualType = actualType.Elem()
	}
	o.customTypeSchemas[actualType] = schema
	// log.Printf("Registered custom type %s with custom schema", actualType.String())
	return nil
}

func (o *OpenAPIGen) Generate() (string, error) {
	err := o.doc.Validate(context.Background())
	if err != nil {
		return "", fmt.Errorf("openapi validation error: %w", err)
	}

	jsonBytes, err := json.MarshalIndent(o.doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("failed to marshal openapi document to json: %w", err)
	}

	return string(jsonBytes), nil
}

func (o *OpenAPIGen) WriteToFile(filename string) error {
	err := o.doc.Validate(context.Background())
	if err != nil {
		return fmt.Errorf("openapi validation error: %w", err)
	}

	jsonBytes, err := json.MarshalIndent(o.doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal openapi document to json: %w", err)
	}

	return os.WriteFile(filename, jsonBytes, 0644)
}
