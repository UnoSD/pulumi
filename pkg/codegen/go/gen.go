// Copyright 2016-2020, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Pulling out some of the repeated strings tokens into constants would harm readability, so we just ignore the
// goconst linter's warning.
//
// nolint: lll, goconst
package gen

import (
	"bytes"
	"fmt"
	"go/format"
	"io"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi/pkg/v2/codegen"
	"github.com/pulumi/pulumi/pkg/v2/codegen/schema"
	"github.com/pulumi/pulumi/sdk/v2/go/common/util/contract"
)

type typeDetails struct {
	ptrElement   bool
	arrayElement bool
	mapElement   bool
}

// Title converts the input string to a title case
// where only the initial letter is upper-cased.
// It also removes $-prefix if any.
func Title(s string) string {
	if s == "" {
		return ""
	}
	if s[0] == '$' {
		return Title(s[1:])
	}
	runes := []rune(s)
	return string(append([]rune{unicode.ToUpper(runes[0])}, runes[1:]...))
}

func camel(s string) string {
	if s == "" {
		return ""
	}
	runes := []rune(s)
	res := make([]rune, 0, len(runes))
	for i, r := range runes {
		if unicode.IsLower(r) {
			res = append(res, runes[i:]...)
			break
		}
		res = append(res, unicode.ToLower(r))
	}
	return string(res)
}

func tokenToPackage(pkg *schema.Package, overrides map[string]string, tok string) string {
	mod := pkg.TokenToModule(tok)
	if override, ok := overrides[mod]; ok {
		mod = override
	}
	return strings.ToLower(mod)
}

type pkgContext struct {
	pkg            *schema.Package
	mod            string
	importBasePath string
	typeDetails    map[*schema.ObjectType]*typeDetails
	enumDetails    map[*schema.EnumType]*typeDetails
	enums          []*schema.EnumType
	types          []*schema.ObjectType
	resources      []*schema.Resource
	functions      []*schema.Function
	names          codegen.StringSet
	renamed        map[string]string
	functionNames  map[*schema.Function]string
	needsUtils     bool
	tool           string
	packages       map[string]*pkgContext

	// Name overrides set in GoPackageInfo
	modToPkg         map[string]string // Module name -> package name
	pkgImportAliases map[string]string // Package name -> import alias
}

func (pkg *pkgContext) detailsForType(t *schema.ObjectType) *typeDetails {
	details, ok := pkg.typeDetails[t]
	if !ok {
		details = &typeDetails{}
		pkg.typeDetails[t] = details
	}
	return details
}

func (pkg *pkgContext) detailsForEnum(e *schema.EnumType) *typeDetails {
	details, ok := pkg.enumDetails[e]
	if !ok {
		details = &typeDetails{}
		pkg.enumDetails[e] = details
	}
	return details
}

func (pkg *pkgContext) tokenToPackage(tok string) string {
	return tokenToPackage(pkg.pkg, pkg.modToPkg, tok)
}

func (pkg *pkgContext) tokenToType(tok string) string {
	// token := pkg : module : member
	// module := path/to/module

	components := strings.Split(tok, ":")
	contract.Assert(len(components) == 3)
	if pkg == nil {
		panic(fmt.Errorf("pkg is nil. token %s", tok))
	}
	if pkg.pkg == nil {
		panic(fmt.Errorf("pkg.pkg is nil. token %s", tok))
	}

	mod, name := pkg.tokenToPackage(tok), components[2]

	modPkg, ok := pkg.packages[mod]
	name = Title(name)

	if ok {
		newName, renamed := modPkg.renamed[name]
		if renamed {
			name = newName
		} else if modPkg.names.Has(name) {
			// If the package containing the type's token already has a resource with the
			// same name, add a `Type` suffix.
			newName = name + "Type"
			modPkg.renamed[name] = newName
			modPkg.names.Add(newName)
			name = newName
		}
	}

	if mod == pkg.mod {
		return name
	}
	if mod == "" {
		mod = components[0]
	}
	mod = strings.Replace(mod, "/", "", -1) + "." + name
	return strings.Replace(mod, "-provider", "", -1)
}

func (pkg *pkgContext) tokenToResource(tok string) string {
	// token := pkg : module : member
	// module := path/to/module

	components := strings.Split(tok, ":")
	contract.Assert(len(components) == 3)
	if pkg == nil {
		panic(fmt.Errorf("pkg is nil. token %s", tok))
	}
	if pkg.pkg == nil {
		panic(fmt.Errorf("pkg.pkg is nil. token %s", tok))
	}

	// Is it a provider resource?
	if components[0] == "pulumi" && components[1] == "providers" {
		return fmt.Sprintf("%s.Provider", components[2])
	}

	mod, name := pkg.tokenToPackage(tok), components[2]

	name = Title(name)

	if mod == pkg.mod {
		return name
	}
	if mod == "" {
		mod = components[0]
	}
	return strings.Replace(mod, "/", "", -1) + "." + name
}

func tokenToModule(tok string) string {
	// token := pkg : module : member
	// module := path/to/module

	components := strings.Split(tok, ":")
	contract.Assert(len(components) == 3)
	return components[1]
}

func tokenToName(tok string) string {
	components := strings.Split(tok, ":")
	contract.Assert(len(components) == 3)
	return Title(components[2])
}

func resourceName(r *schema.Resource) string {
	if r.IsProvider {
		return "Provider"
	}
	return tokenToName(r.Token)
}

func (pkg *pkgContext) plainType(t schema.Type, optional bool) string {
	var typ string
	switch t := t.(type) {
	case *schema.EnumType:
		return pkg.plainType(t.ElementType, optional)
	case *schema.ArrayType:
		typ = "[]"
		if pkg.isExternalReference(t.ElementType) {
			typ += "*"
		}
		typ += pkg.plainType(t.ElementType, false)
		return typ
	case *schema.MapType:
		typ = "map[string]"
		if pkg.isExternalReference(t.ElementType) {
			typ += "*"
		}
		typ += pkg.plainType(t.ElementType, false)
		return typ
	case *schema.ObjectType:
		typ = pkg.resolveObjectType(t)
	case *schema.ResourceType:
		typ = pkg.resolveResourceType(t)
	case *schema.TokenType:
		// Use the underlying type for now.
		if t.UnderlyingType != nil {
			return pkg.plainType(t.UnderlyingType, optional)
		}
		typ = pkg.tokenToType(t.Token)
	case *schema.UnionType:
		// If the union is actually a relaxed enum type, use the underlying
		// type for the enum instead
		for _, e := range t.ElementTypes {
			if typ, ok := e.(*schema.EnumType); ok {
				return pkg.plainType(typ.ElementType, optional)
			}
		}
		// TODO(pdg): union types
		return "interface{}"
	default:
		switch t {
		case schema.BoolType:
			typ = "bool"
		case schema.IntType:
			typ = "int"
		case schema.NumberType:
			typ = "float64"
		case schema.StringType:
			typ = "string"
		case schema.ArchiveType:
			return "pulumi.Archive"
		case schema.AssetType:
			return "pulumi.AssetOrArchive"
		case schema.JSONType:
			fallthrough
		case schema.AnyType:
			return "interface{}"
		}
	}

	if optional {
		return "*" + typ
	}
	return typ
}

func (pkg *pkgContext) inputType(t schema.Type, optional bool) string {
	var typ string
	switch t := t.(type) {
	case *schema.EnumType:
		var prefix string
		if optional {
			prefix = "*"
		}
		// Since enum type is itself an input
		return prefix + pkg.tokenToEnum(t.Token)
	case *schema.ArrayType:
		en := pkg.inputType(t.ElementType, false)
		return strings.TrimSuffix(en, "Input") + "ArrayInput"
	case *schema.MapType:
		en := pkg.inputType(t.ElementType, false)
		return strings.TrimSuffix(en, "Input") + "MapInput"
	case *schema.ObjectType:
		typ = pkg.resolveObjectType(t)
	case *schema.ResourceType:
		typ = pkg.resolveResourceType(t)
		return typ + "Input"
	case *schema.TokenType:
		// Use the underlying type for now.
		if t.UnderlyingType != nil {
			return pkg.inputType(t.UnderlyingType, optional)
		}
		typ = pkg.tokenToType(t.Token)
	case *schema.UnionType:
		// If the union is actually a relaxed enum type, use the underlying
		// type for the input instead
		for _, e := range t.ElementTypes {
			if typ, ok := e.(*schema.EnumType); ok {
				return pkg.inputType(typ.ElementType, optional)
			}
		}
		// TODO(pdg): union types
		return "pulumi.Input"
	default:
		switch t {
		case schema.BoolType:
			typ = "pulumi.Bool"
		case schema.IntType:
			typ = "pulumi.Int"
		case schema.NumberType:
			typ = "pulumi.Float64"
		case schema.StringType:
			typ = "pulumi.String"
		case schema.ArchiveType:
			return "pulumi.ArchiveInput"
		case schema.AssetType:
			return "pulumi.AssetOrArchiveInput"
		case schema.JSONType:
			fallthrough
		case schema.AnyType:
			return "pulumi.Input"
		}
	}

	if optional {
		return typ + "PtrInput"
	}
	return typ + "Input"
}

func (pkg *pkgContext) isExternalReference(t schema.Type) bool {
	switch typ := t.(type) {
	case *schema.ObjectType:
		return typ.Package != nil && pkg.pkg != nil && typ.Package != pkg.pkg
	case *schema.ResourceType:
		return typ.Resource != nil && pkg.pkg != nil && typ.Resource.Package != pkg.pkg
	}
	return false
}

// resolveResourceType resolves resource references in properties while
// taking into account potential external resources. Returned type is
// always marked as required. Caller should check if the property is
// optional and convert the type to a pointer if necessary.
func (pkg *pkgContext) resolveResourceType(t *schema.ResourceType) string {
	if !pkg.isExternalReference(t) {
		return pkg.tokenToResource(t.Token)
	}
	extPkg := t.Resource.Package
	var goInfo GoPackageInfo

	contract.AssertNoError(extPkg.ImportLanguages(map[string]schema.Language{"go": Importer}))
	if info, ok := extPkg.Language["go"].(GoPackageInfo); ok {
		goInfo = info
	}
	extPkgCtx := &pkgContext{
		pkg:              extPkg,
		importBasePath:   goInfo.ImportBasePath,
		pkgImportAliases: goInfo.PackageImportAliases,
		modToPkg:         goInfo.ModuleToPackage,
	}
	resType := extPkgCtx.tokenToResource(t.Token)
	if !strings.Contains(resType, ".") {
		resType = fmt.Sprintf("%s.%s", extPkg.Name, resType)
	}
	return resType
}

// resolveObjectType resolves resource references in properties while
// taking into account potential external resources. Returned type is
// always marked as required. Caller should check if the property is
// optional and convert the type to a pointer if necessary.
func (pkg *pkgContext) resolveObjectType(t *schema.ObjectType) string {
	if !pkg.isExternalReference(t) {
		return pkg.tokenToType(t.Token)
	}
	extPkg := t.Package
	var goInfo GoPackageInfo

	contract.AssertNoError(extPkg.ImportLanguages(map[string]schema.Language{"go": Importer}))
	if info, ok := extPkg.Language["go"].(GoPackageInfo); ok {
		goInfo = info
	}
	extPkgCtx := &pkgContext{
		pkg:              extPkg,
		importBasePath:   goInfo.ImportBasePath,
		pkgImportAliases: goInfo.PackageImportAliases,
		modToPkg:         goInfo.ModuleToPackage,
	}
	return extPkgCtx.plainType(t, false)
}

func (pkg *pkgContext) outputType(t schema.Type, optional bool) string {
	var typ string
	switch t := t.(type) {
	case *schema.EnumType:
		return pkg.outputType(t.ElementType, optional)
	case *schema.ArrayType:
		en := strings.TrimSuffix(pkg.outputType(t.ElementType, false), "Output")
		if en == "pulumi.Any" {
			return "pulumi.ArrayOutput"
		}
		return en + "ArrayOutput"
	case *schema.MapType:
		en := strings.TrimSuffix(pkg.outputType(t.ElementType, false), "Output")
		if en == "pulumi.Any" {
			return "pulumi.MapOutput"
		}
		return en + "MapOutput"
	case *schema.ObjectType:
		typ = pkg.resolveObjectType(t)
	case *schema.ResourceType:
		typ = pkg.resolveResourceType(t)
		return typ + "Output"
	case *schema.TokenType:
		// Use the underlying type for now.
		if t.UnderlyingType != nil {
			return pkg.outputType(t.UnderlyingType, optional)
		}
		typ = pkg.tokenToType(t.Token)
	case *schema.UnionType:
		// If the union is actually a relaxed enum type, use the underlying
		// type for the output instead
		for _, e := range t.ElementTypes {
			if typ, ok := e.(*schema.EnumType); ok {
				return pkg.outputType(typ.ElementType, optional)
			}
		}
		// TODO(pdg): union types
		return "pulumi.AnyOutput"
	default:
		switch t {
		case schema.BoolType:
			typ = "pulumi.Bool"
		case schema.IntType:
			typ = "pulumi.Int"
		case schema.NumberType:
			typ = "pulumi.Float64"
		case schema.StringType:
			typ = "pulumi.String"
		case schema.ArchiveType:
			return "pulumi.ArchiveOutput"
		case schema.AssetType:
			return "pulumi.AssetOrArchiveOutput"
		case schema.JSONType:
			fallthrough
		case schema.AnyType:
			return "pulumi.AnyOutput"
		}
	}

	if optional {
		return typ + "PtrOutput"
	}
	return typ + "Output"
}

func printComment(w io.Writer, comment string, indent bool) int {
	comment = codegen.FilterExamples(comment, "go")

	lines := strings.Split(comment, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for _, l := range lines {
		if indent {
			fmt.Fprintf(w, "\t")
		}
		if l == "" {
			fmt.Fprintf(w, "//\n")
		} else {
			fmt.Fprintf(w, "// %s\n", l)
		}
	}
	return len(lines)
}

func printCommentWithDeprecationMessage(w io.Writer, comment, deprecationMessage string, indent bool) {
	lines := printComment(w, comment, indent)
	if deprecationMessage != "" {
		if lines > 0 {
			fmt.Fprintf(w, "//\n")
		}
		printComment(w, fmt.Sprintf("Deprecated: %s", deprecationMessage), indent)
	}
}

func genInputInterface(w io.Writer, name string) {
	printComment(w, getInputUsage(name), false)
	fmt.Fprintf(w, "type %sInput interface {\n", name)
	fmt.Fprintf(w, "\tpulumi.Input\n\n")
	fmt.Fprintf(w, "\tTo%sOutput() %sOutput\n", Title(name), name)
	fmt.Fprintf(w, "\tTo%sOutputWithContext(context.Context) %sOutput\n", Title(name), name)
	fmt.Fprintf(w, "}\n\n")
}

func getInputUsage(name string) string {
	if strings.HasSuffix(name, "Array") {
		baseTypeName := name[:strings.LastIndex(name, "Array")]
		return strings.Join([]string{
			fmt.Sprintf("%sInput is an input type that accepts %s and %sOutput values.", name, name, name),
			fmt.Sprintf("You can construct a concrete instance of `%sInput` via:", name),
			"",
			fmt.Sprintf("\t\t %s{ %sArgs{...} }", name, baseTypeName),
			" ",
		}, "\n")

	}

	if strings.HasSuffix(name, "Map") {
		baseTypeName := name[:strings.LastIndex(name, "Map")]
		return strings.Join([]string{
			fmt.Sprintf("%sInput is an input type that accepts %s and %sOutput values.", name, name, name),
			fmt.Sprintf("You can construct a concrete instance of `%sInput` via:", name),
			"",
			fmt.Sprintf("\t\t %s{ \"key\": %sArgs{...} }", name, baseTypeName),
			" ",
		}, "\n")
	}

	if strings.HasSuffix(name, "Ptr") {
		baseTypeName := name[:strings.LastIndex(name, "Ptr")]
		return strings.Join([]string{
			fmt.Sprintf("%sInput is an input type that accepts %sArgs, %s and %sOutput values.", name, baseTypeName, name, name),
			fmt.Sprintf("You can construct a concrete instance of `%sInput` via:", name),
			"",
			fmt.Sprintf("\t\t %sArgs{...}", baseTypeName),
			"",
			" or:",
			"",
			"\t\t nil",
			" ",
		}, "\n")
	}

	return strings.Join([]string{
		fmt.Sprintf("%sInput is an input type that accepts %sArgs and %sOutput values.", name, name, name),
		fmt.Sprintf("You can construct a concrete instance of `%sInput` via:", name),
		"",
		fmt.Sprintf("\t\t %sArgs{...}", name),
		" ",
	}, "\n")
}

// genResourceContainerInput handles generating container (slice/map) wrappers around
// resources to facilitate external references.
func genResourceContainerInput(w io.Writer, name, receiverType, elementType string) {
	fmt.Fprintf(w, "func (%s) ElementType() reflect.Type {\n", receiverType)
	fmt.Fprintf(w, "\treturn reflect.TypeOf((%s)(nil))\n", elementType)
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (i %s) To%sOutput() %sOutput {\n", receiverType, Title(name), name)
	fmt.Fprintf(w, "\treturn i.To%sOutputWithContext(context.Background())\n", Title(name))
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (i %s) To%sOutputWithContext(ctx context.Context) %sOutput {\n", receiverType, Title(name), name)
	if strings.HasSuffix(name, "Ptr") {
		base := name[:len(name)-3]
		fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%sOutput).To%sOutput()\n", base, Title(name))
	} else {
		fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%sOutput)\n", name)
	}
	fmt.Fprintf(w, "}\n\n")
}

func genInputMethods(w io.Writer, name, receiverType, elementType string, ptrMethods, resourceType bool) {
	fmt.Fprintf(w, "func (%s) ElementType() reflect.Type {\n", receiverType)
	if resourceType {
		fmt.Fprintf(w, "\treturn reflect.TypeOf((*%s)(nil))\n", elementType)
	} else {
		fmt.Fprintf(w, "\treturn reflect.TypeOf((*%s)(nil)).Elem()\n", elementType)
	}
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (i %s) To%sOutput() %sOutput {\n", receiverType, Title(name), name)
	fmt.Fprintf(w, "\treturn i.To%sOutputWithContext(context.Background())\n", Title(name))
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (i %s) To%sOutputWithContext(ctx context.Context) %sOutput {\n", receiverType, Title(name), name)
	fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%sOutput)\n", name)
	fmt.Fprintf(w, "}\n\n")

	if ptrMethods {
		fmt.Fprintf(w, "func (i %s) To%sPtrOutput() %sPtrOutput {\n", receiverType, Title(name), name)
		fmt.Fprintf(w, "\treturn i.To%sPtrOutputWithContext(context.Background())\n", Title(name))
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (i %s) To%sPtrOutputWithContext(ctx context.Context) %sPtrOutput {\n", receiverType, Title(name), name)
		if strings.HasSuffix(receiverType, "Args") {
			fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%[1]sOutput).To%[1]sPtrOutputWithContext(ctx)\n", name)
		} else {
			fmt.Fprintf(w, "\treturn pulumi.ToOutputWithContext(ctx, i).(%sPtrOutput)\n", name)
		}
		fmt.Fprintf(w, "}\n\n")
	}
}

func (pkg *pkgContext) genEnum(w io.Writer, enum *schema.EnumType) error {
	return pkg.genEnumType(w, pkg.tokenToEnum(enum.Token), enum)
}

func (pkg *pkgContext) genEnumType(w io.Writer, name string, enumType *schema.EnumType) error {
	mod := pkg.tokenToPackage(enumType.Token)
	modPkg, ok := pkg.packages[mod]
	contract.Assert(ok)
	printCommentWithDeprecationMessage(w, enumType.Comment, "", false)
	elementType := pkg.enumElementType(enumType.ElementType, false)
	fmt.Fprintf(w, "type %s %s\n\n", name, elementType)

	fmt.Fprintln(w, "const (")
	for _, e := range enumType.Elements {
		printCommentWithDeprecationMessage(w, e.Comment, e.DeprecationMessage, true)

		var elementName = e.Name
		if e.Name == "" {
			elementName = fmt.Sprintf("%v", e.Value)
		}
		enumName, err := makeSafeEnumName(elementName, name)
		if err != nil {
			return err
		}
		e.Name = enumName
		contract.Assertf(!modPkg.names.Has(e.Name), "Name collision for enum constant: %s for %s",
			e.Name, enumType.Token)

		switch reflect.TypeOf(e.Value).Kind() {
		case reflect.String:
			fmt.Fprintf(w, "%s = %s(%q)\n", e.Name, name, e.Value)
		default:
			fmt.Fprintf(w, "%s = %s(%v)\n", e.Name, name, e.Value)
		}
	}
	fmt.Fprintln(w, ")")
	inputType := pkg.inputType(enumType, false)
	contract.Assertf(name == inputType,
		"expect inputType (%s) for enums to be the same as enum type (%s)", inputType, enumType)
	pkg.genEnumInputFuncs(w, name, enumType, elementType, inputType)
	return nil
}

func (pkg *pkgContext) enumElementType(t schema.Type, optional bool) string {
	suffix := ""
	if optional {
		suffix = "Ptr"
	}
	switch t {
	case schema.BoolType:
		return "pulumi.Bool" + suffix
	case schema.IntType:
		return "pulumi.Int" + suffix
	case schema.NumberType:
		return "pulumi.Float64" + suffix
	case schema.StringType:
		return "pulumi.String" + suffix
	default:
		// We only expect to support the above element types for enums
		panic(fmt.Sprintf("Invalid enum type: %s", t))
	}
}

func (pkg *pkgContext) genEnumInputFuncs(w io.Writer, typeName string, enum *schema.EnumType, elementType, inputType string) {
	fmt.Fprintln(w)
	asFuncName := Title(strings.Replace(elementType, "pulumi.", "", -1))
	fmt.Fprintf(w, "func (%s) ElementType() reflect.Type {\n", typeName)
	fmt.Fprintf(w, "return reflect.TypeOf((*%s)(nil)).Elem()\n", elementType)
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "func (e %s) To%sOutput() %sOutput {\n", typeName, asFuncName, elementType)
	fmt.Fprintf(w, "return pulumi.ToOutput(%[1]s(e)).(%[1]sOutput)\n", elementType)
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "func (e %[1]s) To%[2]sOutputWithContext(ctx context.Context) %[3]sOutput {\n", typeName, asFuncName, elementType)
	fmt.Fprintf(w, "return pulumi.ToOutputWithContext(ctx, %[1]s(e)).(%[1]sOutput)\n", elementType)
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "func (e %[1]s) To%[2]sPtrOutput() %[3]sPtrOutput {\n", typeName, asFuncName, elementType)
	fmt.Fprintf(w, "return %[1]s(e).To%[2]sPtrOutputWithContext(context.Background())\n", elementType, asFuncName)
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "func (e %[1]s) To%[2]sPtrOutputWithContext(ctx context.Context) %[3]sPtrOutput {\n", typeName, asFuncName, elementType)
	fmt.Fprintf(w, "return %[1]s(e).To%[2]sOutputWithContext(ctx).To%[2]sPtrOutputWithContext(ctx)\n", elementType, asFuncName)
	fmt.Fprintln(w, "}")
	fmt.Fprintln(w)
}

func (pkg *pkgContext) genPlainType(w io.Writer, name, comment, deprecationMessage string,
	properties []*schema.Property) {

	printCommentWithDeprecationMessage(w, comment, deprecationMessage, false)
	fmt.Fprintf(w, "type %s struct {\n", name)
	for _, p := range properties {
		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
		fmt.Fprintf(w, "\t%s %s `pulumi:\"%s\"`\n", Title(p.Name), pkg.plainType(p.Type, !p.IsRequired), p.Name)
	}
	fmt.Fprintf(w, "}\n\n")
}

func (pkg *pkgContext) genInputTypes(w io.Writer, t *schema.ObjectType, details *typeDetails) {
	name := pkg.tokenToType(t.Token)

	// Generate the plain inputs.
	genInputInterface(w, name)

	printComment(w, t.Comment, false)
	fmt.Fprintf(w, "type %sArgs struct {\n", name)
	for _, p := range t.Properties {
		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
		fmt.Fprintf(w, "\t%s %s `pulumi:\"%s\"`\n", Title(p.Name), pkg.inputType(p.Type, !p.IsRequired), p.Name)
	}
	fmt.Fprintf(w, "}\n\n")

	genInputMethods(w, name, name+"Args", name, details.ptrElement, false)

	// Generate the pointer input.
	if details.ptrElement {
		genInputInterface(w, name+"Ptr")

		ptrTypeName := camel(name) + "PtrType"

		fmt.Fprintf(w, "type %s %sArgs\n\n", ptrTypeName, name)

		fmt.Fprintf(w, "func %[1]sPtr(v *%[1]sArgs) %[1]sPtrInput {", name)
		fmt.Fprintf(w, "\treturn (*%s)(v)\n", ptrTypeName)
		fmt.Fprintf(w, "}\n\n")

		genInputMethods(w, name+"Ptr", "*"+ptrTypeName, "*"+name, false, false)
	}

	// Generate the array input.
	if details.arrayElement {
		genInputInterface(w, name+"Array")

		fmt.Fprintf(w, "type %[1]sArray []%[1]sInput\n\n", name)

		genInputMethods(w, name+"Array", name+"Array", "[]"+name, false, false)
	}

	// Generate the map input.
	if details.mapElement {
		genInputInterface(w, name+"Map")

		fmt.Fprintf(w, "type %[1]sMap map[string]%[1]sInput\n\n", name)

		genInputMethods(w, name+"Map", name+"Map", "map[string]"+name, false, false)
	}
}

func genOutputMethods(w io.Writer, name, elementType string, resourceType bool) {
	fmt.Fprintf(w, "func (%sOutput) ElementType() reflect.Type {\n", name)
	if resourceType {
		fmt.Fprintf(w, "\treturn reflect.TypeOf((*%s)(nil))\n", elementType)
	} else {
		fmt.Fprintf(w, "\treturn reflect.TypeOf((*%s)(nil)).Elem()\n", elementType)
	}
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sOutput() %[1]sOutput {\n", name, Title(name))
	fmt.Fprintf(w, "\treturn o\n")
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sOutputWithContext(ctx context.Context) %[1]sOutput {\n", name, Title(name))
	fmt.Fprintf(w, "\treturn o\n")
	fmt.Fprintf(w, "}\n\n")
}

func (pkg *pkgContext) genOutputTypes(w io.Writer, t *schema.ObjectType, details *typeDetails) {
	name := pkg.tokenToType(t.Token)

	printComment(w, t.Comment, false)
	fmt.Fprintf(w, "type %sOutput struct { *pulumi.OutputState }\n\n", name)

	genOutputMethods(w, name, name, false)

	if details.ptrElement {
		fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sPtrOutput() %[1]sPtrOutput {\n", name, Title(name))
		fmt.Fprintf(w, "\treturn o.To%sPtrOutputWithContext(context.Background())\n", Title(name))
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sPtrOutputWithContext(ctx context.Context) %[1]sPtrOutput {\n", name, Title(name))
		fmt.Fprintf(w, "\treturn o.ApplyT(func(v %[1]s) *%[1]s {\n", name)
		fmt.Fprintf(w, "\t\treturn &v\n")
		fmt.Fprintf(w, "\t}).(%sPtrOutput)\n", name)
		fmt.Fprintf(w, "}\n")
	}

	for _, p := range t.Properties {
		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, false)
		outputType, applyType := pkg.outputType(p.Type, !p.IsRequired), pkg.plainType(p.Type, !p.IsRequired)

		propName := Title(p.Name)
		switch strings.ToLower(p.Name) {
		case "elementtype", "issecret":
			propName = "Get" + propName
		}
		fmt.Fprintf(w, "func (o %sOutput) %s() %s {\n", name, propName, outputType)
		fmt.Fprintf(w, "\treturn o.ApplyT(func (v %s) %s { return v.%s }).(%s)\n", name, applyType, Title(p.Name), outputType)
		fmt.Fprintf(w, "}\n\n")
	}

	if details.ptrElement {
		fmt.Fprintf(w, "type %sPtrOutput struct { *pulumi.OutputState }\n\n", name)

		genOutputMethods(w, name+"Ptr", "*"+name, false)

		fmt.Fprintf(w, "func (o %[1]sPtrOutput) Elem() %[1]sOutput {\n", name)
		fmt.Fprintf(w, "\treturn o.ApplyT(func (v *%[1]s) %[1]s { return *v }).(%[1]sOutput)\n", name)
		fmt.Fprintf(w, "}\n\n")

		for _, p := range t.Properties {
			printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, false)
			outputType, applyType := pkg.outputType(p.Type, true), pkg.plainType(p.Type, true)
			deref := ""
			// If the property was required, but the type it needs to return is an explicit pointer type, then we need
			// to derference it.
			if p.IsRequired && applyType[0] == '*' {
				deref = "&"
			}

			fmt.Fprintf(w, "func (o %sPtrOutput) %s() %s {\n", name, Title(p.Name), outputType)
			fmt.Fprintf(w, "\treturn o.ApplyT(func (v *%s) %s {\n", name, applyType)
			fmt.Fprintf(w, "\t\tif v == nil {\n")
			fmt.Fprintf(w, "\t\t\treturn nil\n")
			fmt.Fprintf(w, "\t\t}\n")
			fmt.Fprintf(w, "\t\treturn %sv.%s\n", deref, Title(p.Name))
			fmt.Fprintf(w, "\t}).(%s)\n", outputType)
			fmt.Fprintf(w, "}\n\n")
		}
	}

	if details.arrayElement {
		fmt.Fprintf(w, "type %sArrayOutput struct { *pulumi.OutputState }\n\n", name)

		genOutputMethods(w, name+"Array", "[]"+name, false)

		fmt.Fprintf(w, "func (o %[1]sArrayOutput) Index(i pulumi.IntInput) %[1]sOutput {\n", name)
		fmt.Fprintf(w, "\treturn pulumi.All(o, i).ApplyT(func (vs []interface{}) %s {\n", name)
		fmt.Fprintf(w, "\t\treturn vs[0].([]%s)[vs[1].(int)]\n", name)
		fmt.Fprintf(w, "\t}).(%sOutput)\n", name)
		fmt.Fprintf(w, "}\n\n")
	}

	if details.mapElement {
		fmt.Fprintf(w, "type %sMapOutput struct { *pulumi.OutputState }\n\n", name)

		genOutputMethods(w, name+"Map", "map[string]"+name, false)

		fmt.Fprintf(w, "func (o %[1]sMapOutput) MapIndex(k pulumi.StringInput) %[1]sOutput {\n", name)
		fmt.Fprintf(w, "\treturn pulumi.All(o, k).ApplyT(func (vs []interface{}) %s {\n", name)
		fmt.Fprintf(w, "\t\treturn vs[0].(map[string]%s)[vs[1].(string)]\n", name)
		fmt.Fprintf(w, "\t}).(%sOutput)\n", name)
		fmt.Fprintf(w, "}\n\n")
	}
}

func goPrimitiveValue(value interface{}) (string, error) {
	v := reflect.ValueOf(value)
	if v.Kind() == reflect.Interface {
		v = v.Elem()
	}

	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			return "true", nil
		}
		return "false", nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return strconv.FormatInt(v.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return strconv.FormatUint(v.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(v.Float(), 'f', -1, 64), nil
	case reflect.String:
		return fmt.Sprintf("%q", v.String()), nil
	default:
		return "", errors.Errorf("unsupported default value of type %T", value)
	}
}

func (pkg *pkgContext) getConstValue(cv interface{}) (string, error) {
	var val string
	if cv != nil {
		v, err := goPrimitiveValue(cv)
		if err != nil {
			return "", err
		}
		val = v
	}

	return val, nil
}

func (pkg *pkgContext) getDefaultValue(dv *schema.DefaultValue, t schema.Type) (string, error) {
	var val string
	if dv.Value != nil {
		v, err := goPrimitiveValue(dv.Value)
		if err != nil {
			return "", err
		}
		val = v
	}

	if len(dv.Environment) > 0 {
		pkg.needsUtils = true

		parser, typDefault, typ := "nil", "\"\"", "string"
		switch t.(type) {
		case *schema.ArrayType:
			parser, typDefault, typ = "parseEnvStringArray", "pulumi.StringArray{}", "pulumi.StringArray"
		}
		switch t {
		case schema.BoolType:
			parser, typDefault, typ = "parseEnvBool", "false", "bool"
		case schema.IntType:
			parser, typDefault, typ = "parseEnvInt", "0", "int"
		case schema.NumberType:
			parser, typDefault, typ = "parseEnvFloat", "0.0", "float64"
		}

		if val == "" {
			val = typDefault
		}

		val = fmt.Sprintf("getEnvOrDefault(%s, %s", val, parser)
		for _, e := range dv.Environment {
			val += fmt.Sprintf(", %q", e)
		}
		val = fmt.Sprintf("%s).(%s)", val, typ)
	}

	return val, nil
}

func (pkg *pkgContext) genResource(w io.Writer, r *schema.Resource, generateResourceContainerTypes bool) error {
	name := resourceName(r)

	printCommentWithDeprecationMessage(w, r.Comment, r.DeprecationMessage, false)
	fmt.Fprintf(w, "type %s struct {\n", name)

	switch {
	case r.IsProvider:
		fmt.Fprintf(w, "\tpulumi.ProviderResourceState\n\n")
	case r.IsComponent:
		fmt.Fprintf(w, "\tpulumi.ResourceState\n\n")
	default:
		fmt.Fprintf(w, "\tpulumi.CustomResourceState\n\n")
	}

	var secretProps []string
	for _, p := range r.Properties {
		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
		fmt.Fprintf(w, "\t%s %s `pulumi:\"%s\"`\n", Title(p.Name), pkg.outputType(p.Type, !p.IsRequired), p.Name)

		if p.Secret {
			secretProps = append(secretProps, p.Name)
		}
	}
	fmt.Fprintf(w, "}\n\n")

	// Create a constructor function that registers a new instance of this resource.
	fmt.Fprintf(w, "// New%s registers a new resource with the given unique name, arguments, and options.\n", name)
	fmt.Fprintf(w, "func New%s(ctx *pulumi.Context,\n", name)
	fmt.Fprintf(w, "\tname string, args *%[1]sArgs, opts ...pulumi.ResourceOption) (*%[1]s, error) {\n", name)

	// Ensure required arguments are present.
	hasRequired := false
	for _, p := range r.InputProperties {
		if p.IsRequired {
			hasRequired = true
		}
	}

	// Various validation checks
	fmt.Fprintf(w, "\tif args == nil {\n")
	if !hasRequired {
		fmt.Fprintf(w, "\t\targs = &%sArgs{}\n", name)
	} else {
		fmt.Fprintln(w, "\t\treturn nil, errors.New(\"missing one or more required arguments\")")
	}
	fmt.Fprintf(w, "\t}\n\n")

	// Produce the inputs.
	for _, p := range r.InputProperties {
		switch p.Type.(type) {
		case *schema.EnumType:
			// not a pointer type and already handled above
		default:
			if p.IsRequired {
				fmt.Fprintf(w, "\tif args.%s == nil {\n", Title(p.Name))
				fmt.Fprintf(w, "\t\treturn nil, errors.New(\"invalid value for required argument '%s'\")\n", Title(p.Name))
				fmt.Fprintf(w, "\t}\n")
			}
		}
	}

	for _, p := range r.InputProperties {
		if p.ConstValue != nil {
			v, err := pkg.getConstValue(p.ConstValue)
			if err != nil {
				return err
			}

			t := strings.TrimSuffix(pkg.inputType(p.Type, !p.IsRequired), "Input")
			if t == "pulumi." {
				t = "pulumi.Any"
			}

			fmt.Fprintf(w, "\targs.%s = %s(%s)\n", Title(p.Name), t, v)
		}
		if p.DefaultValue != nil {
			v, err := pkg.getDefaultValue(p.DefaultValue, p.Type)
			if err != nil {
				return err
			}

			t := strings.TrimSuffix(pkg.inputType(p.Type, !p.IsRequired), "Input")
			if t == "pulumi." {
				t = "pulumi.Any"
			}

			switch typ := p.Type.(type) {
			case *schema.EnumType:
				if p.IsRequired {
					switch typ.ElementType {
					// Only string and numeric types are supported for enums
					case schema.StringType:
						fmt.Fprintf(w, "\tif args.%s == \"\" {\n", Title(p.Name))
					case schema.IntType, schema.NumberType:
						fmt.Fprintf(w, "\tif args.%s == 0 {\n", Title(p.Name))
					default:
						contract.Assertf(false, "unxpected type %T for enum: %s", typ, typ.Token)
					}
					fmt.Fprintf(w, "\t\targs.%s = %s(%s)\n", Title(p.Name), t, v)
					fmt.Fprintf(w, "\t}\n")
				} else {
					fmt.Fprintf(w, "\tif args.%s == nil {\n", Title(p.Name))

					// Enum types are themselves inputs so pkg.InputType() returns *<EnumType>
					// when the type is optional. We want the generated code to look like this:
					// e:= <EnumType>(<Default>)
					// args.<Name> = &e
					fmt.Fprintf(w, "\te := %s(%s)\n", pkg.inputType(p.Type, false), v)
					fmt.Fprintf(w, "\t\targs.%s = &e\n", Title(p.Name))
					fmt.Fprintf(w, "\t}\n")
				}
			default:
				fmt.Fprintf(w, "\tif args.%s == nil {\n", Title(p.Name))
				fmt.Fprintf(w, "\t\targs.%s = %s(%s)\n", Title(p.Name), t, v)
				fmt.Fprintf(w, "\t}\n")
			}
		}
	}

	// Set any defined aliases.
	if len(r.Aliases) > 0 {
		fmt.Fprintf(w, "\taliases := pulumi.Aliases([]pulumi.Alias{\n")
		for _, alias := range r.Aliases {
			s := "\t\t{\n"
			if alias.Name != nil {
				s += fmt.Sprintf("\t\t\tName: pulumi.String(%q),\n", *alias.Name)
			}
			if alias.Project != nil {
				s += fmt.Sprintf("\t\t\tProject: pulumi.String(%q),\n", *alias.Project)
			}
			if alias.Type != nil {
				s += fmt.Sprintf("\t\t\tType: pulumi.String(%q),\n", *alias.Type)
			}
			s += "\t\t},\n"
			fmt.Fprint(w, s)
		}
		fmt.Fprintf(w, "\t})\n")
		fmt.Fprintf(w, "\topts = append(opts, aliases)\n")
	}
	// Set any defined additionalSecretOutputs.
	if len(secretProps) > 0 {
		fmt.Fprintf(w, "\tsecrets := pulumi.AdditionalSecretOutputs([]string{\n")
		for _, sp := range secretProps {
			fmt.Fprintf(w, "\t\t\t%q,\n", sp)
		}
		fmt.Fprintf(w, "\t})\n")
		fmt.Fprintf(w, "\topts = append(opts, secrets)\n")
	}

	// Finally make the call to registration.
	fmt.Fprintf(w, "\tvar resource %s\n", name)
	if r.IsComponent {
		fmt.Fprintf(w, "\terr := ctx.RegisterRemoteComponentResource(\"%s\", name, args, &resource, opts...)\n", r.Token)
	} else {
		fmt.Fprintf(w, "\terr := ctx.RegisterResource(\"%s\", name, args, &resource, opts...)\n", r.Token)
	}
	fmt.Fprintf(w, "\tif err != nil {\n")
	fmt.Fprintf(w, "\t\treturn nil, err\n")
	fmt.Fprintf(w, "\t}\n")
	fmt.Fprintf(w, "\treturn &resource, nil\n")
	fmt.Fprintf(w, "}\n\n")

	// Emit a factory function that reads existing instances of this resource.
	if !r.IsProvider && !r.IsComponent {
		fmt.Fprintf(w, "// Get%[1]s gets an existing %[1]s resource's state with the given name, ID, and optional\n", name)
		fmt.Fprintf(w, "// state properties that are used to uniquely qualify the lookup (nil if not required).\n")
		fmt.Fprintf(w, "func Get%s(ctx *pulumi.Context,\n", name)
		fmt.Fprintf(w, "\tname string, id pulumi.IDInput, state *%[1]sState, opts ...pulumi.ResourceOption) (*%[1]s, error) {\n", name)
		fmt.Fprintf(w, "\tvar resource %s\n", name)
		fmt.Fprintf(w, "\terr := ctx.ReadResource(\"%s\", name, id, state, &resource, opts...)\n", r.Token)
		fmt.Fprintf(w, "\tif err != nil {\n")
		fmt.Fprintf(w, "\t\treturn nil, err\n")
		fmt.Fprintf(w, "\t}\n")
		fmt.Fprintf(w, "\treturn &resource, nil\n")
		fmt.Fprintf(w, "}\n\n")

		// Emit the state types for get methods.
		fmt.Fprintf(w, "// Input properties used for looking up and filtering %s resources.\n", name)
		fmt.Fprintf(w, "type %sState struct {\n", camel(name))
		for _, p := range r.Properties {
			printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
			fmt.Fprintf(w, "\t%s %s `pulumi:\"%s\"`\n", Title(p.Name), pkg.plainType(p.Type, true), p.Name)
		}
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "type %sState struct {\n", name)
		for _, p := range r.Properties {
			printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
			fmt.Fprintf(w, "\t%s %s\n", Title(p.Name), pkg.inputType(p.Type, true))
		}
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (%sState) ElementType() reflect.Type {\n", name)
		fmt.Fprintf(w, "\treturn reflect.TypeOf((*%sState)(nil)).Elem()\n", camel(name))
		fmt.Fprintf(w, "}\n\n")
	}

	// Emit the args types.
	fmt.Fprintf(w, "type %sArgs struct {\n", camel(name))
	for _, p := range r.InputProperties {
		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
		fmt.Fprintf(w, "\t%s %s `pulumi:\"%s\"`\n", Title(p.Name), pkg.plainType(p.Type, !p.IsRequired), p.Name)
	}
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "// The set of arguments for constructing a %s resource.\n", name)
	fmt.Fprintf(w, "type %sArgs struct {\n", name)
	for _, p := range r.InputProperties {
		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, true)
		fmt.Fprintf(w, "\t%s %s\n", Title(p.Name), pkg.inputType(p.Type, !p.IsRequired))
	}
	fmt.Fprintf(w, "}\n\n")

	fmt.Fprintf(w, "func (%sArgs) ElementType() reflect.Type {\n", name)
	fmt.Fprintf(w, "\treturn reflect.TypeOf((*%sArgs)(nil)).Elem()\n", camel(name))
	fmt.Fprintf(w, "}\n\n")

	// Emit the resource input type.
	fmt.Fprintf(w, "type %sInput interface {\n", name)
	fmt.Fprintf(w, "\tpulumi.Input\n\n")
	fmt.Fprintf(w, "\tTo%[1]sOutput() %[1]sOutput\n", name)
	fmt.Fprintf(w, "\tTo%[1]sOutputWithContext(ctx context.Context) %[1]sOutput\n", name)
	fmt.Fprintf(w, "}\n\n")

	genInputMethods(w, name, "*"+name, name, generateResourceContainerTypes, true)

	if generateResourceContainerTypes {
		// Emit the resource pointer input type.
		fmt.Fprintf(w, "type %sPtrInput interface {\n", name)
		fmt.Fprintf(w, "\tpulumi.Input\n\n")
		fmt.Fprintf(w, "\tTo%[1]sPtrOutput() %[1]sPtrOutput\n", name)
		fmt.Fprintf(w, "\tTo%[1]sPtrOutputWithContext(ctx context.Context) %[1]sPtrOutput\n", name)
		fmt.Fprintf(w, "}\n\n")
		ptrTypeName := camel(name) + "PtrType"
		fmt.Fprintf(w, "type %s %sArgs\n\n", ptrTypeName, name)
		genInputMethods(w, name+"Ptr", "*"+ptrTypeName, "*"+name, false, true)

		if !r.IsProvider {
			// Generate the resource array input.
			genInputInterface(w, name+"Array")
			fmt.Fprintf(w, "type %[1]sArray []%[1]sInput\n\n", name)
			genResourceContainerInput(w, name+"Array", name+"Array", "[]*"+name)

			// Generate the resource map input.
			genInputInterface(w, name+"Map")
			fmt.Fprintf(w, "type %[1]sMap map[string]%[1]sInput\n\n", name)
			genResourceContainerInput(w, name+"Map", name+"Map", "map[string]*"+name)
		}
	}

	// Emit the resource output type.
	fmt.Fprintf(w, "type %sOutput struct {\n", name)
	fmt.Fprintf(w, "\t*pulumi.OutputState\n")
	fmt.Fprintf(w, "}\n\n")
	genOutputMethods(w, name, name, true)
	fmt.Fprintf(w, "\n")
	if generateResourceContainerTypes {
		fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sPtrOutput() %[1]sPtrOutput {\n", name, Title(name))
		fmt.Fprintf(w, "\treturn o.To%sPtrOutputWithContext(context.Background())\n", Title(name))
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (o %[1]sOutput) To%[2]sPtrOutputWithContext(ctx context.Context) %[1]sPtrOutput {\n", name, Title(name))
		fmt.Fprintf(w, "\treturn o.ApplyT(func(v %[1]s) *%[1]s {\n", name)
		fmt.Fprintf(w, "\t\treturn &v\n")
		fmt.Fprintf(w, "\t}).(%sPtrOutput)\n", name)
		fmt.Fprintf(w, "}\n")
		fmt.Fprintf(w, "\n")

		// Emit the resource pointer output type.
		fmt.Fprintf(w, "type %sOutput struct {\n", name+"Ptr")
		fmt.Fprintf(w, "\t*pulumi.OutputState\n")
		fmt.Fprintf(w, "}\n\n")
		genOutputMethods(w, name+"Ptr", "*"+name, true)

		if !r.IsProvider {
			// Emit the array output type
			fmt.Fprintf(w, "type %sArrayOutput struct { *pulumi.OutputState }\n\n", name)
			genOutputMethods(w, name+"Array", "[]"+name, true)
			fmt.Fprintf(w, "func (o %[1]sArrayOutput) Index(i pulumi.IntInput) %[1]sOutput {\n", name)
			fmt.Fprintf(w, "\treturn pulumi.All(o, i).ApplyT(func (vs []interface{}) %s {\n", name)
			fmt.Fprintf(w, "\t\treturn vs[0].([]%s)[vs[1].(int)]\n", name)
			fmt.Fprintf(w, "\t}).(%sOutput)\n", name)
			fmt.Fprintf(w, "}\n\n")
			// Emit the map output type
			fmt.Fprintf(w, "type %sMapOutput struct { *pulumi.OutputState }\n\n", name)
			genOutputMethods(w, name+"Map", "map[string]"+name, true)
			fmt.Fprintf(w, "func (o %[1]sMapOutput) MapIndex(k pulumi.StringInput) %[1]sOutput {\n", name)
			fmt.Fprintf(w, "\treturn pulumi.All(o, k).ApplyT(func (vs []interface{}) %s {\n", name)
			fmt.Fprintf(w, "\t\treturn vs[0].(map[string]%s)[vs[1].(string)]\n", name)
			fmt.Fprintf(w, "\t}).(%sOutput)\n", name)
			fmt.Fprintf(w, "}\n\n")
		}
	}
	// Register all output types
	fmt.Fprintf(w, "func init() {\n")
	fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sOutput{})\n", name)

	if generateResourceContainerTypes {
		fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sPtrOutput{})\n", name)
		if !r.IsProvider {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sArrayOutput{})\n", name)
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sMapOutput{})\n", name)
		}
	}
	fmt.Fprintf(w, "}\n\n")

	return nil
}

func (pkg *pkgContext) genFunction(w io.Writer, f *schema.Function) {
	// If the function starts with New or Get, it will conflict; so rename them.
	name := pkg.functionNames[f]

	printCommentWithDeprecationMessage(w, f.Comment, f.DeprecationMessage, false)

	// Now, emit the function signature.
	argsig := "ctx *pulumi.Context"
	if f.Inputs != nil {
		argsig = fmt.Sprintf("%s, args *%sArgs", argsig, name)
	}
	var retty string
	if f.Outputs == nil {
		retty = "error"
	} else {
		retty = fmt.Sprintf("(*%sResult, error)", name)
	}
	fmt.Fprintf(w, "func %s(%s, opts ...pulumi.InvokeOption) %s {\n", name, argsig, retty)

	// Make a map of inputs to pass to the runtime function.
	var inputsVar string
	if f.Inputs == nil {
		inputsVar = "nil"
	} else {
		inputsVar = "args"
	}

	// Now simply invoke the runtime function with the arguments.
	var outputsType string
	if f.Outputs == nil {
		outputsType = "struct{}"
	} else {
		outputsType = name + "Result"
	}
	fmt.Fprintf(w, "\tvar rv %s\n", outputsType)
	fmt.Fprintf(w, "\terr := ctx.Invoke(\"%s\", %s, &rv, opts...)\n", f.Token, inputsVar)

	if f.Outputs == nil {
		fmt.Fprintf(w, "\treturn err\n")
	} else {
		// Check the error before proceeding.
		fmt.Fprintf(w, "\tif err != nil {\n")
		fmt.Fprintf(w, "\t\treturn nil, err\n")
		fmt.Fprintf(w, "\t}\n")

		// Return the result.
		fmt.Fprintf(w, "\treturn &rv, nil\n")
	}
	fmt.Fprintf(w, "}\n")

	// If there are argument and/or return types, emit them.
	if f.Inputs != nil {
		fmt.Fprintf(w, "\n")
		pkg.genPlainType(w, fmt.Sprintf("%sArgs", name), f.Inputs.Comment, "", f.Inputs.Properties)
	}
	if f.Outputs != nil {
		fmt.Fprintf(w, "\n")
		pkg.genPlainType(w, fmt.Sprintf("%sResult", name), f.Outputs.Comment, "", f.Outputs.Properties)
	}
}

func (pkg *pkgContext) genType(w io.Writer, obj *schema.ObjectType) {
	pkg.genPlainType(w, pkg.tokenToType(obj.Token), obj.Comment, "", obj.Properties)
	pkg.genInputTypes(w, obj, pkg.detailsForType(obj))
	pkg.genOutputTypes(w, obj, pkg.detailsForType(obj))
}

func (pkg *pkgContext) tokenToEnum(tok string) string {
	// token := pkg : module : member
	// module := path/to/module

	components := strings.Split(tok, ":")
	contract.Assert(len(components) == 3)
	if pkg == nil {
		panic(fmt.Errorf("pkg is nil. token %s", tok))
	}
	if pkg.pkg == nil {
		panic(fmt.Errorf("pkg.pkg is nil. token %s", tok))
	}

	mod, name := pkg.tokenToPackage(tok), components[2]

	modPkg, ok := pkg.packages[mod]
	name = Title(name)

	if ok {
		newName, renamed := modPkg.renamed[name]
		if renamed {
			name = newName
		} else if modPkg.names.Has(name) {
			// If the package containing the enum's token already has a resource with the
			// same name, add a `Enum` suffix.
			newName := name + "Enum"
			modPkg.renamed[name] = newName
			modPkg.names.Add(newName)
			name = newName
		}
	}

	if mod == pkg.mod {
		return name
	}
	if mod == "" {
		mod = components[0]
	}
	return strings.Replace(mod, "/", "", -1) + "." + name
}

func (pkg *pkgContext) genTypeRegistrations(w io.Writer, types []*schema.ObjectType) {
	fmt.Fprintf(w, "func init() {\n")
	for _, obj := range types {
		name, details := pkg.tokenToType(obj.Token), pkg.detailsForType(obj)

		fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sOutput{})\n", name)
		if details.ptrElement {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sPtrOutput{})\n", name)
		}
		if details.arrayElement {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sArrayOutput{})\n", name)
		}
		if details.mapElement {
			fmt.Fprintf(w, "\tpulumi.RegisterOutputType(%sMapOutput{})\n", name)
		}
	}
	fmt.Fprintf(w, "}\n")
}

func (pkg *pkgContext) getTypeImports(t schema.Type, recurse bool, importsAndAliases map[string]string, seen map[schema.Type]struct{}) {
	if _, ok := seen[t]; ok {
		return
	}
	seen[t] = struct{}{}
	switch t := t.(type) {
	case *schema.ArrayType:
		pkg.getTypeImports(t.ElementType, recurse, importsAndAliases, seen)
	case *schema.MapType:
		pkg.getTypeImports(t.ElementType, recurse, importsAndAliases, seen)
	case *schema.ObjectType:
		if t.Package != nil && pkg.pkg != nil && t.Package != pkg.pkg {
			extPkg := t.Package
			var goInfo GoPackageInfo

			contract.AssertNoError(extPkg.ImportLanguages(map[string]schema.Language{"go": Importer}))
			if info, ok := extPkg.Language["go"].(GoPackageInfo); ok {
				goInfo = info
			} else {
				// tests don't include ImportBasePath
				goInfo.ImportBasePath = extractImportBasePath(extPkg)
			}
			extPkgCtx := &pkgContext{
				pkg:              extPkg,
				importBasePath:   goInfo.ImportBasePath,
				pkgImportAliases: goInfo.PackageImportAliases,
				modToPkg:         goInfo.ModuleToPackage,
			}
			mod := extPkgCtx.tokenToPackage(t.Token)
			imp := path.Join(goInfo.ImportBasePath, mod)
			importsAndAliases[imp] = goInfo.PackageImportAliases[imp]
			break
		}
		mod := pkg.tokenToPackage(t.Token)
		if mod != pkg.mod {
			p := path.Join(pkg.importBasePath, mod)
			importsAndAliases[path.Join(pkg.importBasePath, mod)] = pkg.pkgImportAliases[p]
		}

		if recurse {
			for _, p := range t.Properties {
				pkg.getTypeImports(p.Type, recurse, importsAndAliases, seen)
			}
		}
	case *schema.ResourceType:
		if t.Resource != nil && pkg.pkg != nil && t.Resource.Package != pkg.pkg {
			extPkg := t.Resource.Package
			var goInfo GoPackageInfo

			contract.AssertNoError(extPkg.ImportLanguages(map[string]schema.Language{"go": Importer}))
			if info, ok := extPkg.Language["go"].(GoPackageInfo); ok {
				goInfo = info
			} else {
				// tests don't include ImportBasePath
				goInfo.ImportBasePath = extractImportBasePath(extPkg)
			}
			extPkgCtx := &pkgContext{
				pkg:              extPkg,
				importBasePath:   goInfo.ImportBasePath,
				pkgImportAliases: goInfo.PackageImportAliases,
				modToPkg:         goInfo.ModuleToPackage,
			}
			mod := extPkgCtx.tokenToPackage(t.Token)
			imp := path.Join(goInfo.ImportBasePath, mod)
			importsAndAliases[imp] = goInfo.PackageImportAliases[imp]
			break
		}
		mod := pkg.tokenToPackage(t.Token)
		if mod != pkg.mod {
			p := path.Join(pkg.importBasePath, mod)
			importsAndAliases[path.Join(pkg.importBasePath, mod)] = pkg.pkgImportAliases[p]
		}
	case *schema.UnionType:
		for _, e := range t.ElementTypes {
			pkg.getTypeImports(e, recurse, importsAndAliases, seen)
		}
	}
}

func extractImportBasePath(extPkg *schema.Package) string {
	version := extPkg.Version.Major
	var vPath string
	if version > 1 {
		vPath = fmt.Sprintf("/v%d", version)
	}
	return fmt.Sprintf("github.com/pulumi/pulumi-%s/sdk%s/go/%s", extPkg.Name, vPath, extPkg.Name)
}

func (pkg *pkgContext) getImports(member interface{}, importsAndAliases map[string]string) {
	seen := map[schema.Type]struct{}{}
	switch member := member.(type) {
	case *schema.ObjectType:
		pkg.getTypeImports(member, true, importsAndAliases, seen)
	case *schema.ResourceType:
		pkg.getTypeImports(member, true, importsAndAliases, seen)
	case *schema.Resource:
		for _, p := range member.Properties {
			pkg.getTypeImports(p.Type, false, importsAndAliases, seen)
		}
		for _, p := range member.InputProperties {
			pkg.getTypeImports(p.Type, false, importsAndAliases, seen)

			if p.IsRequired {
				importsAndAliases["github.com/pkg/errors"] = ""
			}
		}
	case *schema.Function:
		if member.Inputs != nil {
			pkg.getTypeImports(member.Inputs, true, importsAndAliases, seen)
		}
		if member.Outputs != nil {
			pkg.getTypeImports(member.Outputs, true, importsAndAliases, seen)
		}
	case []*schema.Property:
		for _, p := range member {
			pkg.getTypeImports(p.Type, false, importsAndAliases, seen)
		}
	case *schema.EnumType: // Just need pulumi sdk, see below
	default:
		return
	}

	importsAndAliases["github.com/pulumi/pulumi/sdk/v2/go/pulumi"] = ""
}

func (pkg *pkgContext) genHeader(w io.Writer, goImports []string, importsAndAliases map[string]string) {
	fmt.Fprintf(w, "// *** WARNING: this file was generated by %v. ***\n", pkg.tool)
	fmt.Fprintf(w, "// *** Do not edit by hand unless you're certain you know what you are doing! ***\n\n")

	var pkgName string
	if pkg.mod == "" {
		pkgName = goPackage(pkg.pkg.Name)
	} else {
		pkgName = path.Base(pkg.mod)
	}

	fmt.Fprintf(w, "package %s\n\n", pkgName)

	var imports []string
	if len(importsAndAliases) > 0 {
		for k := range importsAndAliases {
			imports = append(imports, k)
		}
		sort.Strings(imports)

		for i, k := range imports {
			if alias := importsAndAliases[k]; alias != "" {
				imports[i] = fmt.Sprintf(`%s "%s"`, alias, k)
			}
		}
	}

	if len(goImports) > 0 {
		if len(imports) > 0 {
			goImports = append(goImports, "")
		}
		imports = append(goImports, imports...)
	}
	if len(imports) > 0 {
		fmt.Fprintf(w, "import (\n")
		for _, i := range imports {
			if i == "" {
				fmt.Fprintf(w, "\n")
			} else {
				if strings.Contains(i, `"`) { // Imports with aliases already include quotes.
					fmt.Fprintf(w, "\t%s\n", i)
				} else {
					fmt.Fprintf(w, "\t%q\n", i)
				}
			}
		}
		fmt.Fprintf(w, ")\n\n")
	}
}

func (pkg *pkgContext) genConfig(w io.Writer, variables []*schema.Property) error {
	importsAndAliases := map[string]string{"github.com/pulumi/pulumi/sdk/v2/go/pulumi/config": ""}
	pkg.getImports(variables, importsAndAliases)

	pkg.genHeader(w, nil, importsAndAliases)

	for _, p := range variables {
		getfunc := "Get"

		var getType string
		var funcType string
		switch p.Type {
		case schema.BoolType:
			getType, funcType = "bool", "Bool"
		case schema.IntType:
			getType, funcType = "int", "Int"
		case schema.NumberType:
			getType, funcType = "float64", "Float64"
		default:
			getType, funcType = "string", ""
		}

		printCommentWithDeprecationMessage(w, p.Comment, p.DeprecationMessage, false)
		configKey := fmt.Sprintf("\"%s:%s\"", pkg.pkg.Name, camel(p.Name))

		fmt.Fprintf(w, "func Get%s(ctx *pulumi.Context) %s {\n", Title(p.Name), getType)
		if p.DefaultValue != nil {
			defaultValue, err := pkg.getDefaultValue(p.DefaultValue, p.Type)
			if err != nil {
				return err
			}

			fmt.Fprintf(w, "\tv, err := config.Try%s(ctx, %s)\n", funcType, configKey)
			fmt.Fprintf(w, "\tif err == nil {\n")
			fmt.Fprintf(w, "\t\treturn v\n")
			fmt.Fprintf(w, "\t}\n")
			fmt.Fprintf(w, "\treturn %s", defaultValue)
		} else {
			fmt.Fprintf(w, "\treturn config.%s%s(ctx, %s)\n", getfunc, funcType, configKey)
		}
		fmt.Fprintf(w, "}\n")
	}

	return nil
}

// genResourceModule generates a ResourceModule definition and the code to register an instance thereof with the
// Pulumi runtime. The generated ResourceModule supports the deserialization of resource references into fully-
// hydrated Resource instances. If this is the root module, this function also generates a ResourcePackage
// definition and its registration to support rehydrating providers.
func (pkg *pkgContext) genResourceModule(w io.Writer) {
	contract.Assert(len(pkg.resources) != 0)

	basePath := pkg.importBasePath

	// TODO: importBasePath isn't currently set for schemas generated by pulumi-terraform-bridge.
	//		 Remove this once the linked issue is fixed. https://github.com/pulumi/pulumi-terraform-bridge/issues/320
	if len(basePath) == 0 {
		basePath = fmt.Sprintf("github.com/pulumi/pulumi-%[1]s/sdk/v2/go/%[1]s", pkg.pkg.Name)
	}

	imports := map[string]string{
		"github.com/blang/semver":                   "",
		"github.com/pulumi/pulumi/sdk/v2/go/pulumi": "",
	}
	topLevelModule := pkg.mod == ""
	if !topLevelModule {
		imports[basePath] = ""
	}

	pkg.genHeader(w, []string{"fmt"}, imports)

	var provider *schema.Resource
	registrations := codegen.StringSet{}
	if providerOnly := len(pkg.resources) == 1 && pkg.resources[0].IsProvider; providerOnly {
		provider = pkg.resources[0]
	} else {
		fmt.Fprintf(w, "type module struct {\n")
		fmt.Fprintf(w, "\tversion semver.Version\n")
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (m *module) Version() semver.Version {\n")
		fmt.Fprintf(w, "\treturn m.version\n")
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (m *module) Construct(ctx *pulumi.Context, name, typ, urn string) (r pulumi.Resource, err error) {\n")
		fmt.Fprintf(w, "\tswitch typ {\n")
		for _, r := range pkg.resources {
			if r.IsProvider {
				contract.Assert(provider == nil)
				provider = r
				continue
			}

			registrations.Add(tokenToModule(r.Token))
			fmt.Fprintf(w, "\tcase %q:\n", r.Token)
			fmt.Fprintf(w, "\t\tr, err = New%s(ctx, name, nil, pulumi.URN_(urn))\n", resourceName(r))
		}
		fmt.Fprintf(w, "\tdefault:\n")
		fmt.Fprintf(w, "\t\treturn nil, fmt.Errorf(\"unknown resource type: %%s\", typ)\n")
		fmt.Fprintf(w, "\t}\n\n")
		fmt.Fprintf(w, "\treturn\n")
		fmt.Fprintf(w, "}\n\n")
	}

	if provider != nil {
		fmt.Fprintf(w, "type pkg struct {\n")
		fmt.Fprintf(w, "\tversion semver.Version\n")
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (p *pkg) Version() semver.Version {\n")
		fmt.Fprintf(w, "\treturn p.version\n")
		fmt.Fprintf(w, "}\n\n")

		fmt.Fprintf(w, "func (p *pkg) ConstructProvider(ctx *pulumi.Context, name, typ, urn string) (pulumi.ProviderResource, error) {\n")
		fmt.Fprintf(w, "\tif typ != \"pulumi:providers:%s\" {\n", pkg.pkg.Name)
		fmt.Fprintf(w, "\t\treturn nil, fmt.Errorf(\"unknown provider type: %%s\", typ)\n")
		fmt.Fprintf(w, "\t}\n\n")
		fmt.Fprintf(w, "\treturn NewProvider(ctx, name, nil, pulumi.URN_(urn))\n")
		fmt.Fprintf(w, "}\n\n")
	}

	fmt.Fprintf(w, "func init() {\n")
	if topLevelModule {
		fmt.Fprintf(w, "\tversion, err := PkgVersion()\n")
	} else {
		// Some package names contain '-' characters, so grab the name from the base path.
		pkgName := basePath[strings.LastIndex(basePath, "/")+1:]
		fmt.Fprintf(w, "\tversion, err := %s.PkgVersion()\n", pkgName)
	}
	fmt.Fprintf(w, "\tif err != nil {\n")
	fmt.Fprintf(w, "\t\tfmt.Println(\"failed to determine package version. defaulting to v1: %%v\", err)\n")
	fmt.Fprintf(w, "\t}\n")
	if len(registrations) > 0 {
		for _, mod := range registrations.SortedValues() {
			fmt.Fprintf(w, "\tpulumi.RegisterResourceModule(\n")
			fmt.Fprintf(w, "\t\t%q,\n", pkg.pkg.Name)
			fmt.Fprintf(w, "\t\t%q,\n", mod)
			fmt.Fprintf(w, "\t\t&module{version},\n")
			fmt.Fprintf(w, "\t)\n")
		}
	}
	if provider != nil {
		fmt.Fprintf(w, "\tpulumi.RegisterResourcePackage(\n")
		fmt.Fprintf(w, "\t\t%q,\n", pkg.pkg.Name)
		fmt.Fprintf(w, "\t\t&pkg{version},\n")
		fmt.Fprintf(w, "\t)\n")
	}
	fmt.Fprintf(w, "}\n")
}

// generatePackageContextMap groups resources, types, and functions into Go packages.
func generatePackageContextMap(tool string, pkg *schema.Package, goInfo GoPackageInfo) map[string]*pkgContext {
	packages := map[string]*pkgContext{}
	getPkg := func(mod string) *pkgContext {
		pack, ok := packages[mod]
		if !ok {
			pack = &pkgContext{
				pkg:              pkg,
				mod:              mod,
				importBasePath:   goInfo.ImportBasePath,
				typeDetails:      map[*schema.ObjectType]*typeDetails{},
				enumDetails:      map[*schema.EnumType]*typeDetails{},
				names:            codegen.NewStringSet(),
				renamed:          map[string]string{},
				functionNames:    map[*schema.Function]string{},
				tool:             tool,
				modToPkg:         goInfo.ModuleToPackage,
				pkgImportAliases: goInfo.PackageImportAliases,
				packages:         packages,
			}
			packages[mod] = pack
		}
		return pack
	}

	getPkgFromToken := func(token string) *pkgContext {
		return getPkg(tokenToPackage(pkg, goInfo.ModuleToPackage, token))
	}

	if len(pkg.Config) > 0 {
		_ = getPkg("config")
	}

	// For any optional properties, we must generate a pointer type for the corresponding property type.
	// In addition, if the optional property's type is itself an object type, we also need to generate pointer
	// types corresponding to all of it's nested properties, as our accessor methods will lift `nil` into
	// those nested types.
	var markOptionalPropertyTypesAsRequiringPtr func(seen codegen.StringSet, props []*schema.Property, parentOptional bool)
	markOptionalPropertyTypesAsRequiringPtr = func(seen codegen.StringSet, props []*schema.Property, parentOptional bool) {
		for _, p := range props {
			if obj, ok := p.Type.(*schema.ObjectType); ok && (!p.IsRequired || parentOptional) {
				if seen.Has(obj.Token) {
					continue
				}

				seen.Add(obj.Token)
				getPkgFromToken(obj.Token).detailsForType(obj).ptrElement = true
				markOptionalPropertyTypesAsRequiringPtr(seen, obj.Properties, true)
			}
			if enum, ok := p.Type.(*schema.EnumType); ok && (!p.IsRequired || parentOptional) {
				if seen.Has(enum.Token) {
					continue
				}
				seen.Add(enum.Token)
				getPkgFromToken(enum.Token).detailsForEnum(enum).ptrElement = true
			}
		}
	}

	// Use a string set to track object types that have already been processed.
	// This avoids recursively processing the same type. For example, in the
	// Kubernetes package, JSONSchemaProps have properties whose type is itself.
	seenMap := codegen.NewStringSet()
	for _, t := range pkg.Types {
		switch typ := t.(type) {
		case *schema.ArrayType:
			if obj, ok := typ.ElementType.(*schema.ObjectType); ok {
				getPkgFromToken(obj.Token).detailsForType(obj).arrayElement = true
			}
		case *schema.MapType:
			if obj, ok := typ.ElementType.(*schema.ObjectType); ok {
				getPkgFromToken(obj.Token).detailsForType(obj).mapElement = true
			}
		case *schema.ObjectType:
			pkg := getPkgFromToken(typ.Token)
			pkg.types = append(pkg.types, typ)
			markOptionalPropertyTypesAsRequiringPtr(seenMap, typ.Properties, false)
		case *schema.EnumType:
			pkg := getPkgFromToken(typ.Token)
			pkg.enums = append(pkg.enums, typ)
		}
	}

	scanResource := func(r *schema.Resource) {
		pkg := getPkgFromToken(r.Token)
		pkg.resources = append(pkg.resources, r)

		pkg.names.Add(resourceName(r))
		pkg.names.Add(resourceName(r) + "Input")
		pkg.names.Add(resourceName(r) + "Output")
		pkg.names.Add(resourceName(r) + "Args")
		pkg.names.Add(camel(resourceName(r)) + "Args")
		pkg.names.Add("New" + resourceName(r))
		if !r.IsProvider && !r.IsComponent {
			pkg.names.Add(resourceName(r) + "State")
			pkg.names.Add(camel(resourceName(r)) + "State")
			pkg.names.Add("Get" + resourceName(r))
		}

		markOptionalPropertyTypesAsRequiringPtr(seenMap, r.InputProperties, !r.IsProvider)
		markOptionalPropertyTypesAsRequiringPtr(seenMap, r.Properties, !r.IsProvider)
	}

	scanResource(pkg.Provider)
	for _, r := range pkg.Resources {
		scanResource(r)
	}

	for _, f := range pkg.Functions {
		pkg := getPkgFromToken(f.Token)
		pkg.functions = append(pkg.functions, f)

		name := tokenToName(f.Token)
		if pkg.names.Has(name) {
			switch {
			case strings.HasPrefix(name, "New"):
				name = "Create" + name[3:]
			case strings.HasPrefix(name, "Get"):
				name = "Lookup" + name[3:]
			}
		}
		pkg.names.Add(name)
		pkg.functionNames[f] = name

		if f.Inputs != nil {
			pkg.names.Add(name + "Args")
		}
		if f.Outputs != nil {
			pkg.names.Add(name + "Result")
		}
	}

	return packages
}

// LanguageResource is derived from the schema and can be used by downstream codegen.
type LanguageResource struct {
	*schema.Resource

	Alias   string // The package alias (e.g. appsv1)
	Name    string // The resource name (e.g. Deployment)
	Package string // The package name (e.g. github.com/pulumi/pulumi-kubernetes/sdk/v2/go/kubernetes/apps/v1)
}

// LanguageResources returns a map of resources that can be used by downstream codegen. The map
// key is the resource schema token.
func LanguageResources(tool string, pkg *schema.Package) (map[string]LanguageResource, error) {
	resources := map[string]LanguageResource{}

	if err := pkg.ImportLanguages(map[string]schema.Language{"go": Importer}); err != nil {
		return nil, err
	}

	var goPkgInfo GoPackageInfo
	if goInfo, ok := pkg.Language["go"].(GoPackageInfo); ok {
		goPkgInfo = goInfo
	}
	packages := generatePackageContextMap(tool, pkg, goPkgInfo)

	// emit each package
	var pkgMods []string
	for mod := range packages {
		pkgMods = append(pkgMods, mod)
	}
	sort.Strings(pkgMods)

	for _, mod := range pkgMods {
		if mod == "" {
			continue
		}
		pkg := packages[mod]

		for _, r := range pkg.resources {
			packagePath := path.Join(goPkgInfo.ImportBasePath, pkg.mod)
			resources[r.Token] = LanguageResource{
				Resource: r,
				Alias:    goPkgInfo.PackageImportAliases[packagePath],
				Name:     tokenToName(r.Token),
				Package:  packagePath,
			}
		}
	}

	return resources, nil
}

func GeneratePackage(tool string, pkg *schema.Package) (map[string][]byte, error) {
	if err := pkg.ImportLanguages(map[string]schema.Language{"go": Importer}); err != nil {
		return nil, err
	}

	var goPkgInfo GoPackageInfo
	if goInfo, ok := pkg.Language["go"].(GoPackageInfo); ok {
		goPkgInfo = goInfo
	}
	packages := generatePackageContextMap(tool, pkg, goPkgInfo)

	// emit each package
	var pkgMods []string
	for mod := range packages {
		pkgMods = append(pkgMods, mod)
	}
	sort.Strings(pkgMods)

	files := map[string][]byte{}
	setFile := func(relPath, contents string) {
		relPath = path.Join(goPackage(pkg.Name), relPath)
		if _, ok := files[relPath]; ok {
			panic(errors.Errorf("duplicate file: %s", relPath))
		}

		// Run Go formatter on the code before saving to disk
		formattedSource, err := format.Source([]byte(contents))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Invalid content:\n%s\n%s\n", relPath, contents)
			panic(errors.Wrapf(err, "invalid Go source code:\n\n%s\n", relPath))
		}

		files[relPath] = formattedSource
	}

	name := goPackage(pkg.Name)
	for _, mod := range pkgMods {
		pkg := packages[mod]

		// Config, description
		switch mod {
		case "":
			buffer := &bytes.Buffer{}
			if pkg.pkg.Description != "" {
				printComment(buffer, pkg.pkg.Description, false)
				fmt.Fprintf(buffer, "//\n")
			} else {
				fmt.Fprintf(buffer, "// Package %[1]s exports types, functions, subpackages for provisioning %[1]s resources.\n", pkg.pkg.Name)
				fmt.Fprintf(buffer, "//\n")
			}
			fmt.Fprintf(buffer, "package %s\n", name)

			setFile(path.Join(mod, "doc.go"), buffer.String())

		case "config":
			if len(pkg.pkg.Config) > 0 {
				buffer := &bytes.Buffer{}
				if err := pkg.genConfig(buffer, pkg.pkg.Config); err != nil {
					return nil, err
				}

				setFile(path.Join(mod, "config.go"), buffer.String())
			}
		}

		// Resources
		for _, r := range pkg.resources {
			importsAndAliases := map[string]string{}
			pkg.getImports(r, importsAndAliases)

			buffer := &bytes.Buffer{}
			pkg.genHeader(buffer, []string{"context", "reflect"}, importsAndAliases)

			if err := pkg.genResource(buffer, r, goPkgInfo.GenerateResourceContainerTypes); err != nil {
				return nil, err
			}

			setFile(path.Join(mod, camel(resourceName(r))+".go"), buffer.String())
		}

		// Functions
		for _, f := range pkg.functions {
			importsAndAliases := map[string]string{}
			pkg.getImports(f, importsAndAliases)

			buffer := &bytes.Buffer{}
			pkg.genHeader(buffer, nil, importsAndAliases)

			pkg.genFunction(buffer, f)

			setFile(path.Join(mod, camel(tokenToName(f.Token))+".go"), buffer.String())
		}

		// Types
		if len(pkg.types) > 0 {
			importsAndAliases := map[string]string{}
			for _, t := range pkg.types {
				pkg.getImports(t, importsAndAliases)
			}

			buffer := &bytes.Buffer{}
			pkg.genHeader(buffer, []string{"context", "reflect"}, importsAndAliases)

			for _, t := range pkg.types {
				pkg.genType(buffer, t)
			}

			pkg.genTypeRegistrations(buffer, pkg.types)

			setFile(path.Join(mod, "pulumiTypes.go"), buffer.String())
		}

		// Enums
		if len(pkg.enums) > 0 {
			imports := map[string]string{}
			for _, e := range pkg.enums {
				pkg.getImports(e, imports)
			}

			buffer := &bytes.Buffer{}
			pkg.genHeader(buffer, []string{"context", "reflect"}, imports)

			for _, e := range pkg.enums {
				if err := pkg.genEnum(buffer, e); err != nil {
					return nil, err
				}
			}
			setFile(path.Join(mod, "pulumiEnums.go"), buffer.String())
		}

		// Utilities
		if pkg.needsUtils || len(mod) == 0 {
			buffer := &bytes.Buffer{}
			importsAndAliases := map[string]string{
				"github.com/blang/semver":                   "",
				"github.com/pulumi/pulumi/sdk/v2/go/pulumi": "",
			}
			pkg.genHeader(buffer, []string{"fmt", "os", "reflect", "regexp", "strconv", "strings"}, importsAndAliases)

			_, err := fmt.Fprintf(buffer, utilitiesFile, pkg.pkg.Name)
			if err != nil {
				return nil, err
			}

			setFile(path.Join(mod, "pulumiUtilities.go"), buffer.String())
		}

		// If there are resources in this module, register the module with the runtime.
		if len(pkg.resources) != 0 {
			buffer := &bytes.Buffer{}
			pkg.genResourceModule(buffer)

			setFile(path.Join(mod, "init.go"), buffer.String())
		}
	}

	return files, nil
}

// goPackage returns the suggested package name for the given string.
func goPackage(name string) string {
	return strings.Split(name, "-")[0]
}

const utilitiesFile = `
type envParser func(v string) interface{}

func parseEnvBool(v string) interface{} {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil
	}
	return b
}

func parseEnvInt(v string) interface{} {
	i, err := strconv.ParseInt(v, 0, 0)
	if err != nil {
		return nil
	}
	return int(i)
}

func parseEnvFloat(v string) interface{} {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return f
}

func parseEnvStringArray(v string) interface{} {
	var result pulumi.StringArray
	for _, item := range strings.Split(v, ";") {
		result = append(result, pulumi.String(item))
	}
	return result
}

func getEnvOrDefault(def interface{}, parser envParser, vars ...string) interface{} {
	for _, v := range vars {
		if value := os.Getenv(v); value != "" {
			if parser != nil {
				return parser(value)
			}
			return value
		}
	}
	return def
}

// PkgVersion uses reflection to determine the version of the current package.
func PkgVersion() (semver.Version, error) {
	type sentinal struct{}
	pkgPath := reflect.TypeOf(sentinal{}).PkgPath()
	re := regexp.MustCompile("^.*/pulumi-%s/sdk(/v\\d+)?")
	if match := re.FindStringSubmatch(pkgPath); match != nil {
		vStr := match[1]
		if len(vStr) == 0 { // If the version capture group was empty, default to v1.
			return semver.Version{Major: 1}, nil
		}
		return semver.MustParse(fmt.Sprintf("%%s.0.0", vStr[2:])), nil
	}
	return semver.Version{}, fmt.Errorf("failed to determine the package version from %%s", pkgPath)
}
`
