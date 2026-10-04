package config

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

var envKinds = map[reflect.Kind]string{reflect.String: "a string", reflect.Int: "an integer", reflect.Float64: "a number", reflect.Bool: "a bool"}

// ApplyEnv sets each top-level scalar key from a non-empty HARNESS_<KEY> variable.
func (c *Config) ApplyEnv(getenv func(string) string) error {
	v := reflect.ValueOf(c).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		name := "HARNESS_" + strings.ToUpper(key)
		t := f.Type
		if t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		s := getenv(name)
		if s == "" || envKinds[t.Kind()] == "" {
			continue
		}
		val := reflect.New(t)
		if err := setScalar(val.Elem(), s); err != nil {
			return fmt.Errorf("config: %s: want %s", name, envKinds[t.Kind()])
		}
		if f.Type.Kind() != reflect.Pointer {
			val = val.Elem()
		}
		v.Field(i).Set(val)
	}
	return nil
}

func setScalar(dst reflect.Value, s string) error {
	switch dst.Kind() {
	case reflect.Int:
		n, err := strconv.Atoi(s)
		dst.SetInt(int64(n))
		return err
	case reflect.Float64:
		n, err := strconv.ParseFloat(s, 64)
		dst.SetFloat(n)
		return err
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		dst.SetBool(b)
		return err
	}
	dst.SetString(s)
	return nil
}
