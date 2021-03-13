package fields

import (
	"reflect"

	"github.com/jt0/gomer/flect"
	"github.com/jt0/gomer/gomerr"
)

type FieldTool interface {
	Name() string
	Applier(structType reflect.Type, structField reflect.StructField, input interface{}) (Applier, gomerr.Gomerr)
}

var registeredFieldTools []FieldTool

func RegisterFieldTools(tools ...FieldTool) {
	if len(registeredFieldTools)+len(tools) > 100 {
		panic("Too many tools. Max = 100")
	}

alreadyRegistered:
	for _, tool := range tools {
		// Skip over duplicates
		for _, registered := range registeredFieldTools {
			if tool.Name() == registered.Name() {
				continue alreadyRegistered
			}
		}
		registeredFieldTools = append(registeredFieldTools, tool)
	}
}

type Applier interface {
	Apply(structValue reflect.Value, fieldValue reflect.Value, toolContext ToolContext) gomerr.Gomerr
}

type ConfigProvider interface {
	ConfigFor(tool FieldTool, structType reflect.Type, structField reflect.StructField) interface{}
}

var FieldToolConfigProvider ConfigProvider = NewStructTagConfigProvider()

type StructTagConfigProvider struct {
	tagKeyFor map[string]string
}

func NewStructTagConfigProvider() StructTagConfigProvider {
	return StructTagConfigProvider{map[string]string{}}
}

func (s StructTagConfigProvider) WithKey(tagKey string, tool FieldTool) StructTagConfigProvider {
	if tagKey == "" {

	}
	RegisterFieldTools(tool)
	s.tagKeyFor[tool.Name()] = tagKey
	return s
}

func (s StructTagConfigProvider) ConfigFor(tool FieldTool, _ reflect.Type, structField reflect.StructField) interface{} {
	tagKey, ok := s.tagKeyFor[tool.Name()]
	if !ok {
		tagKey = tool.Name()
	}

	var config interface{}
	tagValue, ok := structField.Tag.Lookup(tagKey)
	if ok {
		config = tagValue
	} else {
		config = nil
	}

	return config
}

type FunctionApplier struct {
	Function func(structValue reflect.Value) interface{}
}

func (a FunctionApplier) Apply(structValue reflect.Value, fieldValue reflect.Value, _ ToolContext) gomerr.Gomerr {
	defaultValue := a.Function(structValue)
	if ge := flect.SetValue(fieldValue, defaultValue); ge != nil {
		return gomerr.Configuration("Unable to set field to function result").AddAttribute("FunctionResult", defaultValue).Wrap(ge)
	}
	return nil
}

type ValueApplier struct {
	Value string
}

func (a ValueApplier) Apply(_ reflect.Value, fieldValue reflect.Value, _ ToolContext) gomerr.Gomerr {
	if ge := flect.SetValue(fieldValue, a.Value); ge != nil {
		return gomerr.Configuration("Unable to set field to value").AddAttribute("Value", a.Value).Wrap(ge)
	}
	return nil
}

type ApplyAndTestApplier struct {
	Applier  Applier
	IsValid  func(value reflect.Value) bool
	Fallback Applier
}

var (
	NonZero = func(v reflect.Value) bool { return !v.IsZero() }
)

func (a ApplyAndTestApplier) Apply(structValue reflect.Value, fieldValue reflect.Value, toolContext ToolContext) gomerr.Gomerr {
	var applierGe gomerr.Gomerr

	if a.Applier != nil {
		applierGe = a.Applier.Apply(structValue, fieldValue, toolContext)
	}

	if applierGe == nil && a.IsValid(fieldValue) {
		return nil
	}

	if a.Fallback != nil {
		ge := a.Fallback.Apply(structValue, fieldValue, toolContext)
		if ge != nil {
			return ge.Wrap(applierGe) // Okay if applierGe is nil or not-nil
		}
		return nil
	} else if applierGe != nil {
		return applierGe
	}

	return gomerr.Configuration("Field value failed to validate and no fallback applier is specified.")
}

type ToolContext map[string]interface{}

func (tc ToolContext) Add(key string, value interface{}) ToolContext {
	etc := EnsureContext(tc)
	etc[key] = value
	return etc
}

func (tc ToolContext) IncrementInt(key string, amount int) {
	if cv, ok := tc[key]; !ok {
		tc[key] = amount
	} else if ci, ok := cv.(int); ok {
		tc[key] = ci + amount
	} // Field defaultValue is something other than an int so ignore.
}

func EnsureContext(toolContext ...ToolContext) ToolContext {
	if len(toolContext) > 0 && toolContext[0] != nil {
		return toolContext[0]
	} else {
		return ToolContext{}
	}
}
