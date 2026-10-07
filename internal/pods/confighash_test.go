package pods

import (
	"reflect"
	"slices"
	"testing"

	"github.com/wccomps/battleship/internal/config"
)

// hashedFor lists, for each run-affecting config field, which kinds'
// ConfigHash covers it (TestEveryRunConfigFieldIsClassified requires every
// field). A field no kind lists only shapes planning, which the plan records.
var hashedFor = map[string]map[string][]Kind{
	"Naming": {
		"Pool":   {KindDeploy}, // clones join it
		"VMName": nil, "CloneVMIDBase": nil, "CloneVMIDTeamStride": nil, "TemplateVMIDBase": nil, "TemplateSuffix": nil,
	},
	"Network": {
		"ExtBridge": {KindDeploy}, "IntBridge": {KindDeploy}, "ExtSubnet": {KindDeploy}, "CICustom": {KindDeploy},
	},
	"Deploy": {
		"Storage": {KindDeploy}, "Linked": {KindDeploy}, "DiskMBpsRead": {KindDeploy}, "DiskMBpsWrite": {KindDeploy},
		"SnapshotName": {KindDeploy}, "BaselinePatterns": {KindDeploy},
		"MasterTag": nil, "GPUVGA": nil,
	},
	"Teardown": {
		"ShutdownTimeout": {KindTeardown},
	},
}

func TestEveryRunConfigFieldIsClassified(t *testing.T) {
	cfg := reflect.TypeFor[config.Config]()
	for section, fields := range hashedFor {
		sf, ok := cfg.FieldByName(section)
		if !ok {
			t.Errorf("hashedFor lists config.%s, which doesn't exist", section)
			continue
		}
		for i := range sf.Type.NumField() {
			if _, ok := fields[sf.Type.Field(i).Name]; !ok {
				t.Errorf("config.%s.%s is not classified in hashedFor", section, sf.Type.Field(i).Name)
			}
		}
		for name := range fields {
			if _, ok := sf.Type.FieldByName(name); !ok {
				t.Errorf("hashedFor lists config.%s.%s, which doesn't exist", section, name)
			}
		}
	}
}

func TestConfigHashFollowsClassification(t *testing.T) {
	kinds := []Kind{KindDeploy, KindTeardown, KindReset, KindPower, KindSnapshot}
	for section, fields := range hashedFor {
		for name, hashed := range fields {
			c := config.Default()
			f := reflect.ValueOf(&c).Elem().FieldByName(section).FieldByName(name)
			f.Set(changedValue(t, f))
			for _, kind := range kinds {
				got := ConfigHash(kind, c) != ConfigHash(kind, config.Default())
				if want := slices.Contains(hashed, kind); got != want {
					t.Errorf("%s: changing %s.%s changed the hash: %t, want %t", kind, section, name, got, want)
				}
			}
		}
	}
}

func changedValue(t *testing.T, v reflect.Value) reflect.Value {
	t.Helper()
	out := reflect.New(v.Type()).Elem()
	switch v.Kind() {
	case reflect.String:
		out.SetString(v.String() + "x")
	case reflect.Int, reflect.Int64:
		out.SetInt(v.Int() + 1)
	case reflect.Bool:
		out.SetBool(!v.Bool())
	case reflect.Slice:
		out = reflect.Append(reflect.MakeSlice(v.Type(), 0, v.Len()+1), reflect.ValueOf("x"))
		out = reflect.AppendSlice(out, v)
	default:
		t.Fatalf("no way to change a %s", v.Type())
	}
	return out
}
