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

package transform

import (
	"fmt"

	"github.com/compose-spec/compose-go/v2/tree"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
)

func transformPorts(data any, _ tree.Path) (any, error) {
	switch entries := data.(type) {
	case []any:
		// We process the list instead of individual items here.
		// The reason is that one entry might be mapped to multiple ServicePortConfig.
		// Therefore we take an input of a list and return an output of a list.
		var ports []any
		for _, entry := range entries {
			switch value := entry.(type) {
			case int:
				parsed, err := types.ParsePortConfig(fmt.Sprint(value))
				if err != nil {
					return data, err
				}
				for _, v := range parsed {
					m, err := encode(v)
					if err != nil {
						return nil, err
					}
					ports = append(ports, m)
				}
			case string:
				parsed, err := types.ParsePortConfig(value)
				if err != nil {
					return data, err
				}
				if err != nil {
					return nil, err
				}
				for _, v := range parsed {
					m, err := encode(v)
					if err != nil {
						return nil, err
					}
					ports = append(ports, m)
				}
			case map[string]any:
				ports = append(ports, value)
			default:
				return data, errors.Errorf("invalid type %T for port", value)
			}
		}
		return ports, nil
	default:
		return data, errors.Errorf("invalid type %T for port", entries)
	}
}

func encode(v any) (map[string]any, error) {
	m := map[string]any{}
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:  &m,
		TagName: "yaml",
	})
	if err != nil {
		return nil, err
	}
	err = decoder.Decode(v)
	return m, err
}
