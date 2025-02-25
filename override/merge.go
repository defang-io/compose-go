/*
   Copyright 2020 The Compose Specification Authors.

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

package override

import (
	"fmt"
	"strings"

	"github.com/compose-spec/compose-go/v2/tree"
	"golang.org/x/exp/slices"
)

// Merge applies overrides to a config model
func Merge(right, left map[string]any) (map[string]any, error) {
	merged, err := mergeYaml(right, left, tree.NewPath())
	if err != nil {
		return nil, err
	}
	return merged.(map[string]any), nil
}

type merger func(any, any, tree.Path) (any, error)

// mergeSpecials defines the custom rules applied by compose when merging yaml trees
var mergeSpecials = map[tree.Path]merger{}

func init() {
	mergeSpecials["services.*.logging"] = mergeLogging
	mergeSpecials["services.*.command"] = override
	mergeSpecials["services.*.entrypoint"] = override
	mergeSpecials["services.*.healthcheck.test"] = override
	mergeSpecials["services.*.environment"] = mergeEnvironment
	mergeSpecials["services.*.ulimits.*"] = mergeUlimit
}

// mergeYaml merges map[string]any yaml trees handling special rules
func mergeYaml(e any, o any, p tree.Path) (any, error) {
	for pattern, merger := range mergeSpecials {
		if p.Matches(pattern) {
			merged, err := merger(e, o, p)
			if err != nil {
				return nil, err
			}
			return merged, nil
		}
	}
	switch value := e.(type) {
	case map[string]any:
		other, ok := o.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("cannot override %s", p)
		}
		return mergeMappings(value, other, p)
	case []any:
		other, ok := o.([]any)
		if !ok {
			return nil, fmt.Errorf("cannont override %s", p)
		}
		return append(value, other...), nil
	default:
		return o, nil
	}
}

func mergeMappings(mapping map[string]any, other map[string]any, p tree.Path) (map[string]any, error) {
	for k, v := range other {
		e, ok := mapping[k]
		if !ok || strings.HasPrefix(k, "x-") {
			mapping[k] = v
			continue
		}
		next := p.Next(k)
		merged, err := mergeYaml(e, v, next)
		if err != nil {
			return nil, err
		}
		mapping[k] = merged
	}
	return mapping, nil
}

// logging driver options are merged only when both compose file define the same driver
func mergeLogging(c any, o any, p tree.Path) (any, error) {
	config := c.(map[string]any)
	other := o.(map[string]any)
	// we override logging config if source and override have the same driver set, or none
	d, ok1 := other["driver"]
	o, ok2 := config["driver"]
	if d == o || !ok1 || !ok2 {
		return mergeMappings(config, other, p)
	}
	return other, nil
}

// environment must be first converted into yaml sequence syntax so we can append
func mergeEnvironment(c any, o any, _ tree.Path) (any, error) {
	right := convertIntoSequence(c)
	left := convertIntoSequence(o)
	return append(right, left...), nil
}

func convertIntoSequence(value any) []any {
	switch v := value.(type) {
	case map[string]any:
		seq := make([]any, len(v))
		i := 0
		for k, v := range v {
			if v == nil {
				seq[i] = k
			} else {
				seq[i] = fmt.Sprintf("%s=%s", k, v)
			}
			i++
		}
		slices.SortFunc(seq, func(a, b any) bool {
			return a.(string) < b.(string)
		})
		return seq
	case []any:
		return v
	}
	return nil
}

func mergeUlimit(_ any, o any, p tree.Path) (any, error) {
	over, ismapping := o.(map[string]any)
	if base, ok := o.(map[string]any); ok && ismapping {
		return mergeMappings(base, over, p)
	}
	return o, nil
}

func override(_ any, other any, _ tree.Path) (any, error) {
	return other, nil
}
