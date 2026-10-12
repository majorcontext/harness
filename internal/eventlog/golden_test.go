package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const goldenPath = "testdata/envelopes.golden"

func goldenEvents() []Event {
	return []Event{
		SessionCreated{}, OwnerAcquired{}, SettingsChanged{}, InputAdmitted{}, InputPromoted{}, InputWithdrawn{},
		TurnStarted{}, ItemCompleted{}, TurnSuspended{}, TurnResumed{}, TurnEnded{}, RequestOpened{}, RequestResolved{},
		GoalSet{}, GoalEvaluated{}, GoalChanged{}, CompactionApplied{}, ChildSpawned{}, ChildSettled{}, CommandRecorded{},
		ContextMeasured{}, BackendState{}, ToolResultRetained{},
	}
}

// fill sets every field of v to a deterministic non-zero value named after the field.
func fill(v reflect.Value, name string) { fillDepth(v, name, 0) }

func fillDepth(v reflect.Value, name string, depth int) {
	if depth > 6 {
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(name)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint64:
		v.SetUint(7)
	case reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillDepth(v.Elem(), name, depth+1)
	case reflect.Slice:
		if v.Type() == reflect.TypeFor[json.RawMessage]() {
			v.SetBytes([]byte(`{"a":1}`))
			return
		}
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fillDepth(v.Index(0), name, depth+1)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k, "k")
		if e.Kind() == reflect.Interface {
			e.Set(reflect.ValueOf("v"))
		} else {
			fillDepth(e, name, depth+1)
		}
		v.SetMapIndex(k, e)
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fillDepth(v.Field(i), v.Type().Field(i).Name, depth+1)
			}
		}
	}
}

func goldenLines(t *testing.T) []string {
	var out []string
	for i, zero := range goldenEvents() {
		full := reflect.New(reflect.TypeOf(zero)).Elem()
		fill(full, "x")
		for _, ev := range []Event{zero, full.Interface().(Event)} {
			line, err := Envelope{Seq: uint64(i + 1), Time: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC), Event: ev}.Encode()
			if err != nil {
				t.Fatalf("%s: %v", zero.Kind(), err)
			}
			out = append(out, string(line))
		}
	}
	return out
}

func TestEveryKindKeepsItsWireBytes(t *testing.T) {
	got := goldenLines(t)
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, []byte(strings.Join(got, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.Open(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var want []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		want = append(want, sc.Text())
	}
	if len(got) != len(want) {
		t.Fatalf("encoded %d records, golden holds %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("record %d changed:\n got %s\nwant %s", i, got[i], want[i])
		}
		env, err := Decode([]byte(want[i]))
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		again, err := env.Encode()
		if err != nil || string(again) != want[i] {
			t.Errorf("record %d does not round-trip:\n got %s (%v)\nwant %s", i, again, err, want[i])
		}
	}
}
