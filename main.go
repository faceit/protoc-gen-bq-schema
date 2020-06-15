// Copyright 2014 Google Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// protoc plugin which converts .proto to schema for BigQuery.
// It is spawned by protoc and generates schema for BigQuery, encoded in JSON.
//
// usage:
//  $ bin/protoc --bq-schema_out=path/to/outdir foo.proto
//
package protoc_gen_bq_schema

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"path"
	"strings"

	"github.com/faceit/protoc-gen-bq-schema/protos"
	faceit "github.com/faceit/tracking-event-protos-generated/faceit/tracking/v1"

	"github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	descriptor "github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
)

var (
	globalPkg = &ProtoPackage{
		name:     "",
		parent:   nil,
		children: make(map[string]*ProtoPackage),
		types:    make(map[string]*descriptor.DescriptorProto),
	}
)

// Field describes the schema of a field in BigQuery.
type Field struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Mode        string   `json:"mode"`
	Description string   `json:"description,omitempty"`
	Fields      []*Field `json:"fields,omitempty"`
}

// ProtoPackage describes a package of Protobuf, which is an container of message types.
type ProtoPackage struct {
	name     string
	parent   *ProtoPackage
	children map[string]*ProtoPackage
	types    map[string]*descriptor.DescriptorProto
}

func registerType(pkgName *string, msg *descriptor.DescriptorProto) {
	pkg := globalPkg
	if pkgName != nil {
		for _, node := range strings.Split(*pkgName, ".") {
			if pkg == globalPkg && node == "" {
				// Skips leading "."
				continue
			}
			child, ok := pkg.children[node]
			if !ok {
				child = &ProtoPackage{
					name:     pkg.name + "." + node,
					parent:   pkg,
					children: make(map[string]*ProtoPackage),
					types:    make(map[string]*descriptor.DescriptorProto),
				}
				pkg.children[node] = child
			}
			pkg = child
		}
	}
	pkg.types[msg.GetName()] = msg
}

func (pkg *ProtoPackage) lookupType(name string) (*descriptor.DescriptorProto, bool) {
	if strings.HasPrefix(name, ".") {
		return globalPkg.relativelyLookupType(name[1:len(name)])
	}

	for ; pkg != nil; pkg = pkg.parent {
		if desc, ok := pkg.relativelyLookupType(name); ok {
			return desc, ok
		}
	}
	return nil, false
}

func relativelyLookupNestedType(desc *descriptor.DescriptorProto, name string) (*descriptor.DescriptorProto, bool) {
	components := strings.Split(name, ".")
componentLoop:
	for _, component := range components {
		for _, nested := range desc.GetNestedType() {
			if nested.GetName() == component {
				desc = nested
				continue componentLoop
			}
		}
		glog.Infof("no such nested message %s in %s", component, desc.GetName())
		return nil, false
	}
	return desc, true
}

func (pkg *ProtoPackage) relativelyLookupType(name string) (*descriptor.DescriptorProto, bool) {
	components := strings.SplitN(name, ".", 2)
	switch len(components) {
	case 0:
		glog.V(1).Info("empty message name")
		return nil, false
	case 1:
		found, ok := pkg.types[components[0]]
		return found, ok
	case 2:
		glog.Infof("looking for %s in %s at %s (%v)", components[1], components[0], pkg.name, pkg)
		if child, ok := pkg.children[components[0]]; ok {
			found, ok := child.relativelyLookupType(components[1])
			return found, ok
		}
		if msg, ok := pkg.types[components[0]]; ok {
			found, ok := relativelyLookupNestedType(msg, components[1])
			return found, ok
		}
		glog.V(1).Infof("no such package nor message %s in %s", components[0], pkg.name)
		return nil, false
	default:
		glog.Fatal("not reached")
		return nil, false
	}
}

func (pkg *ProtoPackage) relativelyLookupPackage(name string) (*ProtoPackage, bool) {
	components := strings.Split(name, ".")
	for _, c := range components {
		var ok bool
		pkg, ok = pkg.children[c]
		if !ok {
			return nil, false
		}
	}
	return pkg, true
}

var (
	typeFromWKT = map[string]string{
		".google.protobuf.Int32Value":  "INTEGER",
		".google.protobuf.Int64Value":  "INTEGER",
		".google.protobuf.UInt32Value": "INTEGER",
		".google.protobuf.UInt64Value": "INTEGER",
		".google.protobuf.DoubleValue": "FLOAT",
		".google.protobuf.FloatValue":  "FLOAT",
		".google.protobuf.BoolValue":   "BOOLEAN",
		".google.protobuf.StringValue": "STRING",
		".google.protobuf.BytesValue":  "BYTES",
		".google.protobuf.Duration":    "STRING",
		".google.protobuf.Timestamp":   "TIMESTAMP",
	}
	typeFromFieldType = map[descriptor.FieldDescriptorProto_Type]string{
		descriptor.FieldDescriptorProto_TYPE_DOUBLE: "FLOAT",
		descriptor.FieldDescriptorProto_TYPE_FLOAT:  "FLOAT",

		descriptor.FieldDescriptorProto_TYPE_INT64:    "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_UINT64:   "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_INT32:    "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_UINT32:   "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_FIXED64:  "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_FIXED32:  "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_SFIXED32: "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_SFIXED64: "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_SINT32:   "INTEGER",
		descriptor.FieldDescriptorProto_TYPE_SINT64:   "INTEGER",

		descriptor.FieldDescriptorProto_TYPE_STRING: "STRING",
		descriptor.FieldDescriptorProto_TYPE_BYTES:  "BYTES",
		descriptor.FieldDescriptorProto_TYPE_ENUM:   "STRING",

		descriptor.FieldDescriptorProto_TYPE_BOOL: "BOOLEAN",

		descriptor.FieldDescriptorProto_TYPE_GROUP:   "RECORD",
		descriptor.FieldDescriptorProto_TYPE_MESSAGE: "RECORD",
	}

	modeFromFieldLabel = map[descriptor.FieldDescriptorProto_Label]string{
		descriptor.FieldDescriptorProto_LABEL_OPTIONAL: "NULLABLE",
		descriptor.FieldDescriptorProto_LABEL_REQUIRED: "REQUIRED",
		descriptor.FieldDescriptorProto_LABEL_REPEATED: "REPEATED",
	}
)

func convertField(curPkg *ProtoPackage, desc *descriptor.FieldDescriptorProto, msgOpts *protos.BigQueryMessageOptions) (*Field, error) {
	field := &Field{
		Name: desc.GetName(),
	}
	if msgOpts.GetUseJsonNames() && desc.GetJsonName() != "" {
		field.Name = desc.GetJsonName()
	}

	var ok bool
	field.Mode, ok = modeFromFieldLabel[desc.GetLabel()]
	if !ok {
		return nil, fmt.Errorf("unrecognized field label: %s", desc.GetLabel().String())
	}

	field.Type, ok = typeFromFieldType[desc.GetType()]
	if !ok {
		return nil, fmt.Errorf("unrecognized field type: %s", desc.GetType().String())
	}

	opts := desc.GetOptions()
	if opts != nil && proto.HasExtension(opts, protos.E_Bigquery) {
		rawOpt, err := proto.GetExtension(opts, protos.E_Bigquery)
		if err != nil {
			return nil, err
		}
		opt := *rawOpt.(*protos.BigQueryFieldOptions)
		if opt.Ignore {
			// skip the field below
			return nil, nil
		}

		if opt.Require {
			field.Mode = "REQUIRED"
		}

		if len(opt.TypeOverride) > 0 {
			field.Type = opt.TypeOverride
		}

		if len(opt.Name) > 0 {
			field.Name = opt.Name
		}

		if len(opt.Description) > 0 {
			field.Description = opt.Description
		}
	}

	if field.Type != "RECORD" {
		return field, nil
	}
	if t, ok := typeFromWKT[desc.GetTypeName()]; ok {
		field.Type = t
		return field, nil
	}

	recordType, ok := curPkg.lookupType(desc.GetTypeName())
	if !ok {
		return nil, fmt.Errorf("no such message type named %s", desc.GetTypeName())
	}
	fieldMsgOpts, err := getBigqueryMessageOptions(recordType)
	if err != nil {
		return nil, err
	}
	field.Fields, err = convertMessageType(curPkg, recordType, fieldMsgOpts)
	if err != nil {
		return nil, err
	}

	if len(field.Fields) == 0 { // discard RECORDs that would have zero fields
		return nil, nil
	}

	return field, nil
}

func convertMessageType(curPkg *ProtoPackage, msg *descriptor.DescriptorProto, opts *protos.BigQueryMessageOptions) (schema []*Field, err error) {
	if glog.V(4) {
		glog.Info("Converting message: ", proto.MarshalTextString(msg))
	}

	for _, fieldDesc := range msg.GetField() {
		field, err := convertField(curPkg, fieldDesc, opts)
		if err != nil {
			glog.Errorf("Failed to convert field %s in %s: %v", fieldDesc.GetName(), msg.GetName(), err)
			return nil, err
		}

		// if we got no error and the field is nil, skip it
		if field != nil {
			schema = append(schema, field)
		}
	}
	return
}

// NB: This is what the extension for tag 1021 used to look like. For some
// level of backwards compatibility, we will try to parse the extension using
// this definition if we get an error trying to parse it as the current
// definition (a message, to support multiple extension fields therein).
var e_TableName = &proto.ExtensionDesc{
	ExtendedType:  (*descriptor.MessageOptions)(nil),
	ExtensionType: (*string)(nil),
	Field:         1021,
	Name:          "gen_bq_schema.table_name",
	Tag:           "bytes,1021,opt,name=table_name,json=tableName",
	Filename:      "bq_table.proto",
}

func convertFile(file *descriptor.FileDescriptorProto) ([]*plugin.CodeGeneratorResponse_File, error) {
	name := path.Base(file.GetName())
	pkg, ok := globalPkg.relativelyLookupPackage(file.GetPackage())
	if !ok {
		return nil, fmt.Errorf("no such package found: %s", file.GetPackage())
	}

	response := []*plugin.CodeGeneratorResponse_File{}
	for _, msg := range file.GetMessageType() {
		opts, err := getBigqueryMessageOptions(msg)
		if err != nil {
			return nil, err
		}
		if opts == nil {
			continue
		}

		tableName := opts.GetTableName()
		if len(tableName) == 0 {
			continue
		}

		glog.V(2).Info("Generating schema for a message type ", msg.GetName())
		schema, err := convertMessageType(pkg, msg, opts)
		if err != nil {
			glog.Errorf("Failed to convert %s: %v", name, err)
			return nil, err
		}

		jsonSchema, err := json.MarshalIndent(schema, "", " ")
		if err != nil {
			glog.Error("Failed to encode schema", err)
			return nil, err
		}

		resFile := &plugin.CodeGeneratorResponse_File{
			Name:    proto.String(fmt.Sprintf("%s/%s.schema", strings.Replace(file.GetPackage(), ".", "/", -1), tableName)),
			Content: proto.String(string(jsonSchema)),
		}
		response = append(response, resFile)
	}

	return response, nil
}

// getBigqueryMessageOptions returns the bigquery options for the given message.
// If an error is encountered, it is returned instead. If no error occurs, but
// the message has no gen_bq_schema.bigquery_opts option, this function returns
// nil, nil.
func getBigqueryMessageOptions(msg *descriptor.DescriptorProto) (*protos.BigQueryMessageOptions, error) {
	options := msg.GetOptions()
	if options == nil {
		return nil, nil
	}

	if !proto.HasExtension(options, faceit.E_EventName) || !proto.HasExtension(options, faceit.E_EventVersion) {
		return nil, nil
	}

	eventName, err := proto.GetExtension(options, faceit.E_EventName)
	if eventName == "" {
		return nil, err
	}

	eventVersion, err := proto.GetExtension(options, faceit.E_EventVersion)
	if err != nil {
		return nil, err
	}

	name := fmt.Sprintf("%s_v%d", eventName, eventVersion)
	return &protos.BigQueryMessageOptions{
		TableName: name,
	}, nil
}

func Convert(req *plugin.CodeGeneratorRequest) (*plugin.CodeGeneratorResponse, error) {
	generateTargets := make(map[string]bool)
	for _, file := range req.GetFileToGenerate() {
		generateTargets[file] = true
	}

	res := &plugin.CodeGeneratorResponse{}
	for _, file := range req.GetProtoFile() {
		for _, msg := range file.GetMessageType() {
			glog.V(1).Infof("Loading a message type %s from package %s", msg.GetName(), file.GetPackage())
			registerType(file.Package, msg)
		}
	}
	for _, file := range req.GetProtoFile() {
		if _, ok := generateTargets[file.GetName()]; ok {
			glog.V(1).Info("Converting ", file.GetName())
			converted, err := convertFile(file)
			if err != nil {
				res.Error = proto.String(fmt.Sprintf("Failed to convert %s: %v", file.GetName(), err))
				return res, err
			}
			res.File = append(res.File, converted...)
		}
	}
	return res, nil
}

func convertFrom(rd io.Reader) (*plugin.CodeGeneratorResponse, error) {
	glog.V(1).Info("Reading code generation request")
	input, err := ioutil.ReadAll(rd)
	if err != nil {
		glog.Error("Failed to read request:", err)
		return nil, err
	}
	req := &plugin.CodeGeneratorRequest{}
	err = proto.Unmarshal(input, req)
	if err != nil {
		glog.Error("Can't unmarshal input:", err)
		return nil, err
	}

	glog.V(1).Info("Converting input")
	return Convert(req)
}
