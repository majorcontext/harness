package eventlog

import (
	"bufio"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/majorcontext/harness/protocol"
)

const goldenPath = "testdata/envelopes.golden"

func goldenEvents() []protocol.Payload {
	return []protocol.Payload{
		protocol.SessionCreated{}, protocol.OwnerAcquired{}, protocol.SettingsChanged{}, protocol.InputAdmitted{}, protocol.InputPromoted{}, protocol.InputWithdrawn{},
		protocol.TurnStarted{}, protocol.ItemCompleted{}, protocol.TurnSuspended{}, protocol.TurnResumed{}, protocol.TurnEnded{}, protocol.RequestOpened{}, protocol.RequestResolved{},
		protocol.GoalSet{}, protocol.GoalEvaluated{}, protocol.GoalChanged{}, protocol.CompactionApplied{}, protocol.ChildSpawned{}, protocol.ChildSettled{}, protocol.CommandRecorded{},
		protocol.ContextMeasured{}, protocol.BackendState{}, protocol.ToolResultRetained{},
	}
}

func goldenKinds() map[string]bool {
	out := map[string]bool{}
	for _, e := range goldenEvents() {
		out[e.Kind()] = true
	}
	return out
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

// fillEmpty sets every slice and map of v to an empty value that is not nil, and allocates every pointer.
func fillEmpty(v reflect.Value, depth int) {
	if depth > 6 {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillEmpty(v.Elem(), depth+1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() && v.Type() != reflect.TypeFor[time.Time]() {
				fillEmpty(v.Field(i), depth+1)
			}
		}
	}
}

func goldenLines(t *testing.T) []string {
	var out []string
	for i, zero := range goldenEvents() {
		full := reflect.New(reflect.TypeOf(zero)).Elem()
		fill(full, "x")
		empty := reflect.New(reflect.TypeOf(zero)).Elem()
		fillEmpty(empty, 0)
		for _, ev := range []protocol.Payload{zero, full.Interface().(protocol.Payload), empty.Interface().(protocol.Payload)} {
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
	defer func() { _ = f.Close() }()
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
