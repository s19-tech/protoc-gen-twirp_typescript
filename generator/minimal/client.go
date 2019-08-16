package minimal

import (
	"bytes"
	"fmt"
	"log"
	"path"
	"strings"
	"text/template"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
)

const apiTemplate = `
import {createTwirpRequest, throwTwirpError, Fetch} from './twirp';

{{- range .Enums}}

export enum {{.Name}} {
{{- range .Values}}
	{{.}} = "{{.}}",
{{- end}}
}
{{- end -}}

{{- range .Models -}}
{{- if not .Primitive}}

export interface {{.Name}} {
{{- if .IsMap}}
	[key: string]: {{.MapValueType}};
{{- else -}}
{{- range .Fields}}
    {{.Name}}?: {{.Type}};
{{- end}}
{{- end}}
}

interface {{.Name}}JSON {
{{- if .IsMap}}
{{- if .MapValueTypePrimitive}}
	[key: string]: {{.MapValueType}};
{{- else}}
	[key: string]: {{.MapValueType}}JSON;
{{- end -}}
{{- else -}}
{{- range .Fields}}
	{{.JSONName}}?: {{.JSONType}};
{{- end}}
{{- end}}
}

{{- if not .MapValueTypePrimitive}}
{{- if .CanMarshal}}

const {{.Name}}ToJSON = (m: {{.Name}}): {{.Name}}JSON => {
{{- if .IsMap}}
	return Object.keys(m).reduce((acc, key) => {
		acc[key] = {{.MapValueType}}ToJSON(m[key]);
		return acc;
	}, {} as {{.Name}});
{{- else}}
    return {
        {{- range .Fields}}
        {{.JSONName}}: {{stringify .}},
        {{- end}}
	};
{{- end}}
};
{{- end -}}

{{- if .CanUnmarshal}}

const JSONTo{{.Name}} = (m: {{.Name}}JSON): {{.Name}} => {
	{{- $Model := .Name -}}
{{- if .IsMap}}
	return Object.keys(m).reduce((acc, key) => {
		acc[key] = JSONTo{{.MapValueType}}(m[key]);
		return acc;
	  }, {} as {{.Name}});
{{- else}}
    return {
        {{- range .Fields}}
        {{.Name}}: {{parse . $Model}},
        {{- end}}
	};
{{- end}}
};
{{- end -}}
{{end -}}
{{end -}}
{{end -}}

{{- $twirpPrefix := .TwirpPrefix -}}

{{range .Services}}

export interface {{.Name}} {
{{- range .Methods}}
    {{.Name}}: ({{.InputArg}}: {{.InputType}}) => Promise<{{.OutputType}}>;
{{- end}}
}

export class {{.Name}}Client implements {{.Name}} {
    private hostname: string;
    private fetch: Fetch;
    private writeCamelCase: boolean;
	private pathPrefix = "{{$twirpPrefix}}/{{.Package}}.{{.Name}}/";
	private optionsOverride: object;

    constructor(hostname: string, fetch: Fetch, writeCamelCase = false, optionsOverride: any = {}) {
        this.hostname = hostname;
        this.fetch = fetch;
		this.writeCamelCase = writeCamelCase;
		this.optionsOverride = optionsOverride;
    }

{{- range .Methods}}

    {{.Name}}({{.InputArg}}: {{.InputType}}): Promise<{{.OutputType}}> {
        const url = this.hostname + this.pathPrefix + "{{.Path}}";
        let body: {{.InputType}} | {{.InputType}}JSON = {{.InputArg}};
        if (!this.writeCamelCase) {
            body = {{.InputType}}ToJSON({{.InputArg}});
        }
        return this.fetch(createTwirpRequest(url, body, this.optionsOverride)).then((resp) => {
            if (!resp.ok) {
                return throwTwirpError(resp);
            }

            return resp.json().then(JSONTo{{.OutputType}});
        });
    }
{{- end}}
}
{{- end}}
`

type Model struct {
	Name                  string
	Primitive             bool
	Fields                []ModelField
	CanMarshal            bool
	CanUnmarshal          bool
	IsMap                 bool
	MapValueType          string
	MapValueTypePrimitive bool
}

type ModelField struct {
	Name                  string
	Type                  string
	JSONName              string
	JSONType              string
	IsMessage             bool
	IsRepeated            bool
	IsMap                 bool
	MapValueTypePrimitive bool
}

type Service struct {
	Name    string
	Package string
	Methods []ServiceMethod
}

type ServiceMethod struct {
	Name       string
	Path       string
	InputArg   string
	InputType  string
	OutputType string
}

type Enum struct {
	Name   string
	Values []string
}

func NewAPIContext(twirpVersion string) APIContext {
	twirpPrefix := "/twirp"
	if twirpVersion == "v6" {
		twirpPrefix = ""
	}

	ctx := APIContext{TwirpPrefix: twirpPrefix}
	ctx.modelLookup = make(map[string]*Model)

	return ctx
}

type APIContext struct {
	Package     string
	Models      []*Model
	Services    []*Service
	Enums       []*Enum
	TwirpPrefix string
	modelLookup map[string]*Model
}

func (ctx *APIContext) AddModel(m *Model) {
	ctx.Models = append(ctx.Models, m)
	ctx.modelLookup[m.Name] = m
}

func getBaseType(f ModelField) string {
	baseType := f.Type
	if f.IsRepeated {
		baseType = strings.Trim(baseType, "[]")
	}

	return baseType
}

// ApplyMarshalFlags will inspect the CanMarshal and CanUnmarshal flags for models where
// the flags are enabled and recursively set the same values on all the models that are field types.
func (ctx *APIContext) ApplyMarshalFlags() {
	for _, m := range ctx.Models {
		for _, f := range m.Fields {
			// skip primitive types and WKT Timestamps
			if !f.IsMessage || f.Type == "Date" {
				continue
			}

			baseType := getBaseType(f)
			if m.CanMarshal {
				ctx.enableMarshal(ctx.modelLookup[baseType])
			}

			if m.CanUnmarshal {
				m, ok := ctx.modelLookup[baseType]
				if !ok {
					log.Fatalf("could not find model of type %s for field %s", baseType, f.Name)
				}
				ctx.enableUnmarshal(m)
			}
		}
	}
}

func (ctx *APIContext) enableMarshal(m *Model) {
	m.CanMarshal = true

	for _, f := range m.Fields {
		// skip primitive types and WKT Timestamps
		if !f.IsMessage || f.Type == "Date" {
			continue
		}

		baseType := getBaseType(f)

		mm, ok := ctx.modelLookup[baseType]
		if !ok {
			log.Fatalf("could not find model of type %s for field %s", f.Type, f.Name)
		}
		ctx.enableMarshal(mm)
	}
}

func (ctx *APIContext) enableUnmarshal(m *Model) {
	m.CanUnmarshal = true

	for _, f := range m.Fields {
		// skip primitive types and WKT Timestamps
		if !f.IsMessage || f.Type == "Date" {
			continue
		}
		baseType := getBaseType(f)

		mm, ok := ctx.modelLookup[baseType]
		if !ok {
			log.Fatalf("could not find model of type %s for field %s", f.Type, f.Name)
		}
		ctx.enableUnmarshal(mm)
	}
}

func NewGenerator(twirpVersion string, p map[string]string) *Generator {
	return &Generator{twirpVersion: twirpVersion, params: p}
}

type Generator struct {
	twirpVersion string
	params       map[string]string
}

func (g *Generator) Generate(d *descriptor.FileDescriptorProto) ([]*plugin.CodeGeneratorResponse_File, error) {
	var files []*plugin.CodeGeneratorResponse_File

	// skip WKT Timestamp, we don't do any special serialization for jsonpb.
	if *d.Name == "google/protobuf/timestamp.proto" {
		return files, nil
	}

	ctx := NewAPIContext(g.twirpVersion)
	ctx.Package = d.GetPackage()

	// TODO: This whole parsing code needs refactoring.
	// It only supports one level of nesting which is done by duplicating
	// code rather than using recursion

	// Parse all enums for generating tpescript
	for _, e := range d.GetEnumType() {
		enum := &Enum{
			Name: e.GetName(),
		}
		for _, ev := range e.GetValue() {
			enum.Values = append(enum.Values, ev.GetName())
		}

		ctx.Enums = append(ctx.Enums, enum)
	}

	// Parse all Messages for generating typescript interfaces
	for _, m := range d.GetMessageType() {
		model := &Model{
			Name: m.GetName(),
		}

		// Parse all nested enums
		for _, e := range m.GetEnumType() {
			enum := &Enum{
				Name: fmt.Sprintf("%s_%s", m.GetName(), e.GetName()),
			}
			for _, ev := range e.GetValue() {
				enum.Values = append(enum.Values, ev.GetName())
			}

			ctx.Enums = append(ctx.Enums, enum)
		}

		// Parse all nested models
		for _, m2 := range m.GetNestedType() {
			nestedModel := &Model{
				Name: fmt.Sprintf("%s_%s", m.GetName(), m2.GetName()),
			}

			if m2.Options.GetMapEntry() {
				nestedModel.IsMap = true
			}

			for _, f2 := range m2.GetField() {
				mf := ctx.newField(f2)
				if nestedModel.IsMap && mf.Name == "value" {
					nestedModel.MapValueType = mf.Type
					if f2.GetType() != descriptor.FieldDescriptorProto_TYPE_MESSAGE {
						nestedModel.MapValueTypePrimitive = true
					}
				}
				nestedModel.Fields = append(nestedModel.Fields, mf)

			}

			ctx.AddModel(nestedModel)
		}

		for _, f := range m.GetField() {
			f3 := ctx.newField(f)

			ml, ok := ctx.modelLookup[ctx.removePkg(f.GetTypeName())]
			if ok && ml.IsMap && ml.MapValueTypePrimitive {
				f3.MapValueTypePrimitive = true
			}

			model.Fields = append(model.Fields, f3)
		}

		ctx.AddModel(model)
	}

	// Parse all Services for generating typescript method interfaces and default client implementations
	for _, s := range d.GetService() {
		service := &Service{
			Name:    s.GetName(),
			Package: ctx.Package,
		}

		for _, m := range s.GetMethod() {
			methodPath := m.GetName()
			methodName := strings.ToLower(methodPath[0:1]) + methodPath[1:]
			in := ctx.removePkg(m.GetInputType())
			arg := strings.ToLower(in[0:1]) + in[1:]

			method := ServiceMethod{
				Name:       methodName,
				Path:       methodPath,
				InputArg:   arg,
				InputType:  in,
				OutputType: ctx.removePkg(m.GetOutputType()),
			}

			service.Methods = append(service.Methods, method)
		}

		ctx.Services = append(ctx.Services, service)
	}

	// Only include the custom 'ToJSON' and 'JSONTo' methods in generated code
	// if the Model is part of an rpc method input arg or return type.
	for _, m := range ctx.Models {
		for _, s := range ctx.Services {
			for _, sm := range s.Methods {
				if m.Name == sm.InputType {
					m.CanMarshal = true
				}

				if m.Name == sm.OutputType {
					m.CanUnmarshal = true
				}
			}
		}
	}

	ctx.AddModel(&Model{
		Name:      "Date",
		Primitive: true,
	})

	ctx.ApplyMarshalFlags()

	funcMap := template.FuncMap{
		"stringify": stringify,
		"parse":     parse,
	}

	t, err := template.New("client_api").Funcs(funcMap).Parse(apiTemplate)
	if err != nil {
		return nil, err
	}

	b := bytes.NewBufferString("")
	err = t.Execute(b, ctx)
	if err != nil {
		return nil, err
	}

	clientAPI := &plugin.CodeGeneratorResponse_File{}
	clientAPI.Name = proto.String(tsModuleFilename(d))
	clientAPI.Content = proto.String(b.String())

	files = append(files, clientAPI)
	files = append(files, RuntimeLibrary())

	if pkgName, ok := g.params["package_name"]; ok {
		idx, err := CreatePackageIndex(files)
		if err != nil {
			return nil, err
		}

		files = append(files, idx)
		files = append(files, CreateTSConfig())
		files = append(files, CreatePackageJSON(pkgName))
	}

	return files, nil
}

func tsModuleFilename(f *descriptor.FileDescriptorProto) string {
	name := *f.Name

	if ext := path.Ext(name); ext == ".proto" || ext == ".protodevel" {
		base := path.Base(name)
		name = base[:len(base)-len(path.Ext(base))]
	}

	name += ".ts"

	return name
}

func (c *APIContext) newField(f *descriptor.FieldDescriptorProto) ModelField {
	field := ModelField{
		Name: camelCase(f.GetName()),
	}

	if m, ok := c.modelLookup[c.removePkg(f.GetTypeName())]; ok {
		field.IsMap = m.IsMap
	}

	field.Type, field.JSONType = c.protoToTSType(f, field)
	field.JSONName = f.GetName()
	field.IsMessage = f.GetType() == descriptor.FieldDescriptorProto_TYPE_MESSAGE && !(f.GetTypeName() == ".google.protobuf.Timestamp")
	field.IsRepeated = isRepeated(f)

	return field
}

// generates the (Type, JSONType) tuple for a ModelField so marshal/unmarshal functions
// will work when converting between TS interfaces and protobuf JSON.
func (c *APIContext) protoToTSType(f *descriptor.FieldDescriptorProto, mf ModelField) (string, string) {
	tsType := "string"
	jsonType := "string"

	switch f.GetType() {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE,
		descriptor.FieldDescriptorProto_TYPE_FIXED32,
		descriptor.FieldDescriptorProto_TYPE_FIXED64,
		descriptor.FieldDescriptorProto_TYPE_INT32,
		descriptor.FieldDescriptorProto_TYPE_INT64:
		tsType = "number"
		jsonType = "number"
	case descriptor.FieldDescriptorProto_TYPE_STRING:
		tsType = "string"
		jsonType = "string"
	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		tsType = "boolean"
		jsonType = "boolean"
	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		name := f.GetTypeName()

		// Google WKT Timestamp is a special case here:
		//
		// Currently the value will just be left as jsonpb RFC 3339 string.
		// JSON.stringify already handles serializing Date to its RFC 3339 format.
		//
		if name == ".google.protobuf.Timestamp" {
			tsType = "string"
			jsonType = "string"
		} else {
			tsType = c.removePkg(name)
			jsonType = c.removePkg(name) + "JSON"
		}
	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		name := f.GetTypeName()
		tsType = c.removePkg(name)
		jsonType = c.removePkg(name)
	}

	if isRepeated(f) && !mf.IsMap {
		tsType = tsType + "[]"
		jsonType = jsonType + "[]"
	}

	return tsType, jsonType
}

func isRepeated(field *descriptor.FieldDescriptorProto) bool {
	return field.Label != nil && *field.Label == descriptor.FieldDescriptorProto_LABEL_REPEATED
}

func (c *APIContext) removePkg(s string) string {
	s2 := strings.ReplaceAll(s, c.Package, "")
	s3 := strings.TrimLeft(s2, ".")
	return strings.ReplaceAll(s3, ".", "_")
}

func camelCase(s string) string {
	parts := strings.Split(s, "_")

	for i, p := range parts {
		if i == 0 {
			parts[i] = strings.ToLower(p)
		} else {
			parts[i] = strings.ToUpper(p[0:1]) + strings.ToLower(p[1:])
		}
	}

	return strings.Join(parts, "")
}

func stringify(f ModelField) string {
	if f.IsRepeated && !f.IsMap {
		singularType := strings.Trim(f.Type, "[]") // strip array brackets from type

		if f.Type == "Date" {
			return fmt.Sprintf("m.%s && m.%s.map((n) => n.toISOString())", f.Name, f.Name)
		}

		if f.IsMessage {
			return fmt.Sprintf("m.%s && m.%s.map(%sToJSON)", f.Name, f.Name, singularType)
		}
	}

	if f.Type == "Date" {
		return fmt.Sprintf("m.%s && m.%s.toISOString()", f.Name, f.Name)
	}

	if f.IsMessage && !f.MapValueTypePrimitive {
		return fmt.Sprintf("m.%s && %sToJSON(m.%s)", f.Name, f.Type, f.Name)
	}

	return "m." + f.Name
}

func parse(f ModelField, modelName string) string {
	field := "m." + f.JSONName

	if f.IsRepeated && !f.IsMap {
		singularTSType := strings.Trim(f.Type, "[]") // strip array brackets from type

		if f.Type == "Date[]" {
			return fmt.Sprintf("%s && %s.map((n) => new Date(n))", field, field)
		}

		if f.IsMessage {
			return fmt.Sprintf("%s && %s.map(JSONTo%s)", field, field, singularTSType)
		}
	}

	if f.Type == "Date" {
		return fmt.Sprintf("%s && new Date(%s)", field, field)
	}

	if f.IsMessage && !f.MapValueTypePrimitive {
		return fmt.Sprintf("%s && JSONTo%s(%s)", field, f.Type, field)
	}

	return field
}
