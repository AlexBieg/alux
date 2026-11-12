package agent

import (
	"reflect"
	"strings"
	"testing"

	"github.com/alexbieg/alux/internal/tools"
)

// A schema property with no matching json tag decodes to nothing, so the model's flag would be dropped without a word.
func TestEverySearchSchemaPropertyDecodes(t *testing.T) {
	tags := map[string]bool{}
	typ := reflect.TypeOf(tools.SearchInput{})
	for i := range typ.NumField() {
		tags[strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	for _, tool := range []*struct {
		name  string
		props map[string]any
	}{
		{"grep", grepTool.InputSchema.Properties.(map[string]any)},
		{"glob", globTool.InputSchema.Properties.(map[string]any)},
	} {
		for prop := range tool.props {
			if !tags[prop] {
				t.Errorf("%s schema property %q has no SearchInput field", tool.name, prop)
			}
		}
	}
}
