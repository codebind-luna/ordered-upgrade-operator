/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dag

import (
	"reflect"
	"strings"
	"testing"
)

func TestWaves(t *testing.T) {
	const worker, api = "worker", "api"

	tests := []struct {
		name    string
		names   []string
		deps    map[string][]string
		want    [][]string
		wantErr string
	}{
		{
			name:  "legacy worker/api pair",
			names: []string{worker, api},
			deps:  map[string][]string{api: {worker}},
			want:  [][]string{{worker}, {api}},
		},
		{
			// a calls b and c, b calls d: callees first.
			name:  "diamond-free graph",
			names: []string{"a", "b", "c", "d"},
			deps:  map[string][]string{"a": {"b", "c"}, "b": {"d"}},
			want:  [][]string{{"c", "d"}, {"b"}, {"a"}},
		},
		{
			name:  "shared dependency",
			names: []string{"x", "y", "db"},
			deps:  map[string][]string{"x": {"db"}, "y": {"db"}},
			want:  [][]string{{"db"}, {"x", "y"}},
		},
		{
			name:  "independent components share a wave",
			names: []string{"p", "q"},
			want:  [][]string{{"p", "q"}},
		},
		{
			name:  "duplicate dependency counted once",
			names: []string{"a", "b"},
			deps:  map[string][]string{"a": {"b", "b"}},
			want:  [][]string{{"b"}, {"a"}},
		},
		{
			name:    "cycle",
			names:   []string{"a", "b", "c"},
			deps:    map[string][]string{"a": {"b"}, "b": {"a"}},
			wantErr: "dependency cycle among components: a, b",
		},
		{
			name:    "self dependency",
			names:   []string{"a"},
			deps:    map[string][]string{"a": {"a"}},
			wantErr: "dependency cycle",
		},
		{
			name:    "unknown dependency",
			names:   []string{"a"},
			deps:    map[string][]string{"a": {"ghost"}},
			wantErr: `depends on unknown component "ghost"`,
		},
		{
			name:    "duplicate name",
			names:   []string{"a", "a"},
			wantErr: "listed more than once",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Waves(tt.names, tt.deps)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("waves = %v, want %v", got, tt.want)
			}
		})
	}
}
