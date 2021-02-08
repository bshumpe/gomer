package fields

import (
	"reflect"
	"regexp"

	"github.com/jt0/gomer/gomerr"
)

const ScopeKey = "$_scope"

// Format: [<scope>:]<tool_config>[;[<scope>:]<tool_config>]]*
// Note that both ':' and ';' are special chars. Once a scope has been provided, colons are allowed until the
// end of the input or a ';' is found. If a colon should be used for what would otherwise not contain a scope,
// one can use the wildcard scope (e.g. "*:this_colon_:_does_not_indicate_a_scope").
//
// NB: scopes can't be reused. If a scope repeats, the last one wins. This is true for wildcards (implicit,
//     explicit, or both) as well.

var scopeAliases = make(map[string][]string)

// AddScopeAliases allows the caller to define one or more values that may be used as the ScopeKey value when a
// ToolContext is being populated. The alias values, along with the value passed in during a call to New(), will map
// to the same FieldTool definition. Note that scope aliases need to be set up before a struct's fields are
// processed.
func AddScopeAliases(scopesToAliases map[string][]string) {
	for scope, aliases := range scopesToAliases {
		scopeAliases[scope] = append(scopeAliases[scope], aliases...)
	}
}

// ResetScopeAliases removes all scope -> aliases mappings.
func ResetScopeAliases() {
	scopeAliases = make(map[string][]string)
}

type ScopingWrapper struct {
	FieldTool
	scopedTools map[string]FieldTool
}

func (w ScopingWrapper) Name() string {
	return w.FieldTool.Name()
}

var scopeRegexp = regexp.MustCompile("(?:([^;:]*):)?([^;]*);?")

const anyScope = "*"

// Generics will make this better, but for now, we assume input is a string
func (w ScopingWrapper) New(structType reflect.Type, structField reflect.StructField, input interface{}) (FieldTool, gomerr.Gomerr) {
	scopedTools := make(map[string]FieldTool)
	for _, match := range scopeRegexp.FindAllStringSubmatch(input.(string), -1) {
		if remainder := match[2]; remainder != "" {
			scope := match[1]
			if scope == "" {
				scope = anyScope
			}

			tool, ge := w.FieldTool.New(structType, structField, remainder)
			if ge != nil {
				return nil, ge
			}

			scopedTools[scope] = tool
			for _, alias := range scopeAliases[scope] {
				scopedTools[alias] = tool
			}
		}
	}

	return ScopingWrapper{nil, scopedTools}, nil
}

func (w ScopingWrapper) Apply(structValue reflect.Value, fieldValue reflect.Value, toolContext ToolContext) gomerr.Gomerr {
	scope, ok := toolContext[ScopeKey].(string)
	if !ok {
		scope = anyScope
	}

	scopedTool, ok := w.scopedTools[scope]
	if !ok {
		scopedTool, ok = w.scopedTools[anyScope]
		if !ok {
			return nil // no matching tool, return
		}
	}

	return scopedTool.Apply(structValue, fieldValue, toolContext)
}

func AddScopeToContext(scope string, tcs ...ToolContext) ToolContext {
	tc := EnsureContext(tcs...)
	tc[ScopeKey] = scope
	return tc
}
