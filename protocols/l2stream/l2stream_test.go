package l2stream

import (
	"reflect"
	"strings"
	"testing"
)

// The jsonschema tag splits on commas: a comma would cut a description short.
func TestDescriptionsHaveNoCommas(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(Config{}), reflect.TypeOf(EchoConfig{})} {
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("jsonschema")
			if _, d, ok := strings.Cut(tag, "description="); ok && strings.Contains(d, ",") {
				t.Errorf("%s.%s: comma in %q", typ.Name(), typ.Field(i).Name, d)
			}
		}
	}
}
