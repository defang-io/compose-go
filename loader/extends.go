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

package loader

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/compose-spec/compose-go/v2/consts"
	"github.com/compose-spec/compose-go/v2/override"
	"github.com/compose-spec/compose-go/v2/types"
)

func ApplyExtends(ctx context.Context, dict map[string]any, workingdir string, opts *Options, ct *cycleTracker, post ...PostProcessor) error {
	a, ok := dict["services"]
	if !ok {
		return nil
	}
	services := a.(map[string]any)
	for name, s := range services {
		service := s.(map[string]any)
		x, ok := service["extends"]
		if !ok {
			continue
		}
		if err := ct.Add(ctx.Value(consts.ComposeFileKey{}).(string), name); err != nil {
			return err
		}
		var (
			ref  string
			file any
		)
		switch v := x.(type) {
		case map[string]any:
			ref = v["service"].(string)
			file = v["file"]
		case string:
			ref = v
		}

		var base any
		if file != nil {
			path := file.(string)
			for _, loader := range opts.ResourceLoaders {
				if !loader.Accept(path) {
					continue
				}
				local, err := loader.Load(ctx, path)
				if err != nil {
					return err
				}
				relworkingdir := filepath.Dir(local)
				if !filepath.IsAbs(local) {
					relworkingdir, err = filepath.Rel(workingdir, relworkingdir)
					if err != nil {
						return err
					}
				}
				extendsOpts := opts.clone()
				extendsOpts.ResolvePaths = true
				extendsOpts.SkipNormalization = true
				extendsOpts.SkipConsistencyCheck = true
				extendsOpts.SkipInclude = true
				source, err := loadYamlModel(ctx, types.ConfigDetails{
					WorkingDir: relworkingdir,
					ConfigFiles: []types.ConfigFile{
						{Filename: local},
					},
				}, extendsOpts, ct, nil)
				if err != nil {
					return err
				}
				services := source["services"].(map[string]any)
				base, ok = services[ref]
				if !ok {
					return fmt.Errorf("cannot extend service %q in %s: service not found", name, path)
				}
			}
			if base == nil {
				return fmt.Errorf("cannot read %s", path)
			}
		} else {
			base, ok = services[ref]
			if !ok {
				return fmt.Errorf("cannot extend service %q in %s: service not found", name, "filename") // TODO track filename
			}
		}
		source := deepClone(base).(map[string]any)
		for _, processor := range post {
			processor.Apply(map[string]any{
				"services": map[string]any{
					name: source,
				},
			})
		}
		merged, err := override.ExtendService(source, service)
		if err != nil {
			return err
		}
		delete(merged, "extends")
		services[name] = merged
	}
	dict["services"] = services
	return nil
}

func deepClone(value any) any {
	switch v := value.(type) {
	case []any:
		cp := make([]any, len(v))
		for i, e := range v {
			cp[i] = deepClone(e)
		}
		return cp
	case map[string]any:
		cp := make(map[string]any, len(v))
		for k, e := range v {
			cp[k] = deepClone(e)
		}
		return cp
	default:
		return value
	}
}
