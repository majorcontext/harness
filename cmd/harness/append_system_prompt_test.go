package main

import (
	"reflect"
	"testing"

	"github.com/majorcontext/harness/config"
)

func TestAppendSystemSegments(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
		flag string
		want []string
	}{
		{"unset", nil, "", nil},
		{"config", &config.Config{AppendSystemPrompt: []string{"one", "two"}}, "", []string{"one", "two"}},
		{"flag", nil, "flag", []string{"flag"}},
		{"both", &config.Config{AppendSystemPrompt: []string{"one", "two"}}, "flag", []string{"one", "two", "flag"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appendSystemSegments(tt.cfg, tt.flag); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("appendSystemSegments = %v, want %v", got, tt.want)
			}
		})
	}
}
