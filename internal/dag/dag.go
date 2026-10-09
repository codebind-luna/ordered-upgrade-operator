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

// Package dag orders the components of an upgrade by their dependencies.
package dag

import (
	"fmt"
	"slices"
	"strings"
)

// Waves groups nodes into upgrade waves with Kahn's algorithm. deps maps a node
// to the nodes it depends on (its callees); a node lands in a wave only once
// every node it depends on is in an earlier one, so wave 0 holds the nodes with
// no dependencies and each later wave can start once the previous ones are done.
//
// Kahn's algorithm runs on the happens-before direction - dependency to
// dependent - which is the reverse of how deps is written, so the edges are
// flipped first. Within a wave, nodes keep the order they have in names, which
// keeps the result deterministic for status output and tests.
//
// It returns an error if a dependency names a node not in names, or if the graph
// has a cycle: nodes still waiting after the queue drains are exactly the ones on
// or behind a cycle, so they are named in the message.
func Waves(names []string, deps map[string][]string) ([][]string, error) {
	position := make(map[string]int, len(names))
	for i, n := range names {
		if _, dup := position[n]; dup {
			return nil, fmt.Errorf("component %q is listed more than once", n)
		}
		position[n] = i
	}

	indegree := make(map[string]int, len(names))        // unfinished dependencies per node
	dependents := make(map[string][]string, len(names)) // reversed edges: callee -> callers
	for _, n := range names {
		seen := map[string]bool{}
		for _, d := range deps[n] {
			if _, ok := position[d]; !ok {
				return nil, fmt.Errorf("component %q depends on unknown component %q", n, d)
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			indegree[n]++
			dependents[d] = append(dependents[d], n)
		}
	}

	var ready []string
	for _, n := range names {
		if indegree[n] == 0 {
			ready = append(ready, n)
		}
	}

	var waves [][]string
	placed := 0
	for len(ready) > 0 {
		wave := ready
		waves = append(waves, wave)
		placed += len(wave)

		ready = nil
		for _, n := range wave {
			for _, m := range dependents[n] {
				indegree[m]--
				if indegree[m] == 0 {
					ready = append(ready, m)
				}
			}
		}
		slices.SortFunc(ready, func(a, b string) int { return position[a] - position[b] })
	}

	if placed != len(names) {
		var stuck []string
		for _, n := range names {
			if indegree[n] > 0 {
				stuck = append(stuck, n)
			}
		}
		return nil, fmt.Errorf("dependency cycle among components: %s", strings.Join(stuck, ", "))
	}
	return waves, nil
}
