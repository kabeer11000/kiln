package kiln

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The schema must describe exactly the fields LoadConfig accepts, at every level.
func TestSchemaMatchesConfig(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal([]byte(Schema), &s); err != nil {
		t.Fatal(err)
	}
	var check func(path string, typ reflect.Type, node map[string]any)
	check = func(path string, typ reflect.Type, node map[string]any) {
		props, _ := node["properties"].(map[string]any)
		var fields []string
		for i := range typ.NumField() {
			f := typ.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			fields = append(fields, name)
			sub, ok := props[name].(map[string]any)
			if !ok {
				t.Errorf("schema is missing %s%s", path, name)
				continue
			}
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				check(path+name+".", ft, sub)
			}
		}
		for name := range props {
			if !slices.Contains(fields, name) {
				t.Errorf("schema has %s%s, which kiln.yaml does not accept", path, name)
			}
		}
		if node["additionalProperties"] != false {
			t.Errorf("schema %s must reject unknown fields, like kiln does", path)
		}
	}
	check("", reflect.TypeFor[Config](), s)
}
