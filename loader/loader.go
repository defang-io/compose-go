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
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/consts"
	interp "github.com/compose-spec/compose-go/v2/interpolation"
	"github.com/compose-spec/compose-go/v2/override"
	"github.com/compose-spec/compose-go/v2/paths"
	"github.com/compose-spec/compose-go/v2/schema"
	"github.com/compose-spec/compose-go/v2/template"
	"github.com/compose-spec/compose-go/v2/transform"
	"github.com/compose-spec/compose-go/v2/tree"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/compose-spec/compose-go/v2/validation"
	"github.com/mitchellh/mapstructure"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

// Options supported by Load
type Options struct {
	// Skip schema validation
	SkipValidation bool
	// Skip interpolation
	SkipInterpolation bool
	// Skip normalization
	SkipNormalization bool
	// Resolve path
	ResolvePaths bool
	// Convert Windows path
	ConvertWindowsPaths bool
	// Skip consistency check
	SkipConsistencyCheck bool
	// Skip extends
	SkipExtends bool
	// SkipInclude will ignore `include` and only load model from file(s) set by ConfigDetails
	SkipInclude bool
	// SkipResolveEnvironment will ignore computing `environment` for services
	SkipResolveEnvironment bool
	// Interpolation options
	Interpolate *interp.Options
	// Discard 'env_file' entries after resolving to 'environment' section
	discardEnvFiles bool
	// Set project projectName
	projectName string
	// Indicates when the projectName was imperatively set or guessed from path
	projectNameImperativelySet bool
	// Profiles set profiles to enable
	Profiles []string
	// ResourceLoaders manages support for remote resources
	ResourceLoaders []ResourceLoader
}

// ResourceLoader is a plugable remote resource resolver
type ResourceLoader interface {
	// Accept returns `true` is the resource reference matches ResourceLoader supported protocol(s)
	Accept(path string) bool
	// Load returns the path to a local copy of remote resource identified by `path`.
	Load(ctx context.Context, path string) (string, error)
}

type localResourceLoader struct {
	WorkingDir string
}

func (l localResourceLoader) abs(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(l.WorkingDir, p)
}

func (l localResourceLoader) Accept(p string) bool {
	_, err := os.Stat(l.abs(p))
	return err == nil
}

func (l localResourceLoader) Load(_ context.Context, p string) (string, error) {
	return l.abs(p), nil
}

func (o *Options) clone() *Options {
	return &Options{
		SkipValidation:             o.SkipValidation,
		SkipInterpolation:          o.SkipInterpolation,
		SkipNormalization:          o.SkipNormalization,
		ResolvePaths:               o.ResolvePaths,
		ConvertWindowsPaths:        o.ConvertWindowsPaths,
		SkipConsistencyCheck:       o.SkipConsistencyCheck,
		SkipExtends:                o.SkipExtends,
		SkipInclude:                o.SkipInclude,
		Interpolate:                o.Interpolate,
		discardEnvFiles:            o.discardEnvFiles,
		projectName:                o.projectName,
		projectNameImperativelySet: o.projectNameImperativelySet,
		Profiles:                   o.Profiles,
		ResourceLoaders:            o.ResourceLoaders,
	}
}

func (o *Options) SetProjectName(name string, imperativelySet bool) {
	o.projectName = name
	o.projectNameImperativelySet = imperativelySet
}

func (o Options) GetProjectName() (string, bool) {
	return o.projectName, o.projectNameImperativelySet
}

// serviceRef identifies a reference to a service. It's used to detect cyclic
// references in "extends".
type serviceRef struct {
	filename string
	service  string
}

type cycleTracker struct {
	loaded []serviceRef
}

func (ct *cycleTracker) Add(filename, service string) error {
	toAdd := serviceRef{filename: filename, service: service}
	for _, loaded := range ct.loaded {
		if toAdd == loaded {
			// Create an error message of the form:
			// Circular reference:
			//   service-a in docker-compose.yml
			//   extends service-b in docker-compose.yml
			//   extends service-a in docker-compose.yml
			errLines := []string{
				"Circular reference:",
				fmt.Sprintf("  %s in %s", ct.loaded[0].service, ct.loaded[0].filename),
			}
			for _, service := range append(ct.loaded[1:], toAdd) {
				errLines = append(errLines, fmt.Sprintf("  extends %s in %s", service.service, service.filename))
			}

			return errors.New(strings.Join(errLines, "\n"))
		}
	}

	ct.loaded = append(ct.loaded, toAdd)
	return nil
}

// WithDiscardEnvFiles sets the Options to discard the `env_file` section after resolving to
// the `environment` section
func WithDiscardEnvFiles(opts *Options) {
	opts.discardEnvFiles = true
}

// WithSkipValidation sets the Options to skip validation when loading sections
func WithSkipValidation(opts *Options) {
	opts.SkipValidation = true
}

// WithProfiles sets profiles to be activated
func WithProfiles(profiles []string) func(*Options) {
	return func(opts *Options) {
		opts.Profiles = profiles
	}
}

// ParseYAML reads the bytes from a file, parses the bytes into a mapping
// structure, and returns it.
func ParseYAML(source []byte) (map[string]interface{}, error) {
	r := bytes.NewReader(source)
	decoder := yaml.NewDecoder(r)
	m, _, err := parseYAML(decoder)
	return m, err
}

// PostProcessor is used to tweak compose model based on metadata extracted during yaml Unmarshal phase
// that hardly can be implemented using go-yaml and mapstructure
type PostProcessor interface {
	yaml.Unmarshaler

	// Apply changes to compose model based on recorder metadata
	Apply(interface{}) error
}

func parseYAML(decoder *yaml.Decoder) (map[string]interface{}, PostProcessor, error) {
	var cfg interface{}
	processor := ResetProcessor{target: &cfg}

	if err := decoder.Decode(&processor); err != nil {
		return nil, nil, err
	}
	stringMap, ok := cfg.(map[string]interface{})
	if ok {
		converted, err := convertToStringKeysRecursive(stringMap, "")
		if err != nil {
			return nil, nil, err
		}
		return converted.(map[string]interface{}), &processor, nil
	}
	cfgMap, ok := cfg.(map[interface{}]interface{})
	if !ok {
		return nil, nil, errors.Errorf("Top-level object must be a mapping")
	}
	converted, err := convertToStringKeysRecursive(cfgMap, "")
	if err != nil {
		return nil, nil, err
	}
	return converted.(map[string]interface{}), &processor, nil
}

// Load reads a ConfigDetails and returns a fully loaded configuration.
// Deprecated: use LoadWithContext.
func Load(configDetails types.ConfigDetails, options ...func(*Options)) (*types.Project, error) {
	return LoadWithContext(context.Background(), configDetails, options...)
}

// LoadWithContext reads a ConfigDetails and returns a fully loaded configuration
func LoadWithContext(ctx context.Context, configDetails types.ConfigDetails, options ...func(*Options)) (*types.Project, error) {
	if len(configDetails.ConfigFiles) < 1 {
		return nil, errors.Errorf("No files specified")
	}

	opts := &Options{
		Interpolate: &interp.Options{
			Substitute:      template.Substitute,
			LookupValue:     configDetails.LookupEnv,
			TypeCastMapping: interpolateTypeCastMapping,
		},
		ResolvePaths: true,
	}

	for _, op := range options {
		op(opts)
	}
	opts.ResourceLoaders = append(opts.ResourceLoaders, localResourceLoader{configDetails.WorkingDir})

	projectName, err := projectName(configDetails, opts)
	if err != nil {
		return nil, err
	}
	opts.projectName = projectName

	// TODO(milas): this should probably ALWAYS set (overriding any existing)
	if _, ok := configDetails.Environment[consts.ComposeProjectName]; !ok && projectName != "" {
		if configDetails.Environment == nil {
			configDetails.Environment = map[string]string{}
		}
		configDetails.Environment[consts.ComposeProjectName] = projectName
	}

	return load(ctx, configDetails, opts, nil)
}

func loadYamlModel(ctx context.Context, config types.ConfigDetails, opts *Options, ct *cycleTracker, included []string) (map[string]interface{}, error) {
	var (
		dict = map[string]interface{}{}
		err  error
	)
	for _, file := range config.ConfigFiles {
		fctx := context.WithValue(ctx, consts.ComposeFileKey{}, file.Filename)
		if len(file.Content) == 0 && file.Config == nil {
			content, err := os.ReadFile(file.Filename)
			if err != nil {
				return nil, err
			}
			file.Content = content
		}

		processRawYaml := func(raw interface{}, processors ...PostProcessor) error {
			converted, err := convertToStringKeysRecursive(raw, "")
			if err != nil {
				return err
			}
			cfg, ok := converted.(map[string]interface{})
			if !ok {
				return errors.New("Top-level object must be a mapping")
			}

			if opts.Interpolate != nil && !opts.SkipInterpolation {
				cfg, err = interp.Interpolate(cfg, *opts.Interpolate)
				if err != nil {
					return err
				}
			}

			fixEmptyNotNull(cfg)

			if !opts.SkipValidation {
				if err := schema.Validate(cfg); err != nil {
					return fmt.Errorf("validating %s: %w", file.Filename, err)
				}
			}

			if !opts.SkipExtends {
				err = ApplyExtends(fctx, cfg, config.WorkingDir, opts, ct, processors...)
				if err != nil {
					return err
				}
			}

			for _, processor := range processors {
				if err := processor.Apply(dict); err != nil {
					return err
				}
			}

			dict, err = override.Merge(dict, cfg)

			return err
		}

		if file.Config == nil {
			r := bytes.NewReader(file.Content)
			decoder := yaml.NewDecoder(r)
			for {
				var raw interface{}
				processor := &ResetProcessor{target: &raw}
				err := decoder.Decode(processor)
				if err == io.EOF {
					break
				}
				if err := processRawYaml(raw, processor); err != nil {
					return nil, err
				}
			}
		} else {
			if err := processRawYaml(file.Config); err != nil {
				return nil, err
			}
		}
	}

	dict, err = override.EnforceUnicity(dict)
	if err != nil {
		return nil, err
	}

	dict, err = transform.Canonical(dict)
	if err != nil {
		return nil, err
	}

	if !opts.SkipInclude {
		included = append(included, config.ConfigFiles[0].Filename)
		err = ApplyInclude(ctx, config, dict, opts, included)
		if err != nil {
			return nil, err
		}
	}

	dict = groupXFieldsIntoExtensions(dict, tree.NewPath())

	if !opts.SkipValidation {
		if err := validation.Validate(dict); err != nil {
			return nil, err
		}
	}

	if opts.ResolvePaths {
		err = paths.ResolveRelativePaths(dict, config.WorkingDir)
		if err != nil {
			return nil, err
		}
	}

	return dict, nil
}

func load(ctx context.Context, configDetails types.ConfigDetails, opts *Options, loaded []string) (*types.Project, error) {
	mainFile := configDetails.ConfigFiles[0].Filename
	for _, f := range loaded {
		if f == mainFile {
			loaded = append(loaded, mainFile)
			return nil, errors.Errorf("include cycle detected:\n%s\n include %s", loaded[0], strings.Join(loaded[1:], "\n include "))
		}
	}
	loaded = append(loaded, mainFile)

	includeRefs := make(map[string][]types.IncludeConfig)

	dict, err := loadYamlModel(ctx, configDetails, opts, &cycleTracker{}, nil)
	if err != nil {
		return nil, err
	}

	if len(dict) == 0 {
		return nil, errors.New("empty compose file")
	}

	project := &types.Project{
		Name:        opts.projectName,
		WorkingDir:  configDetails.WorkingDir,
		Environment: configDetails.Environment,
	}
	delete(dict, "name") // project name set by yaml must be identified by caller as opts.projectName
	err = Transform(dict, project)
	if err != nil {
		return nil, err
	}

	if len(includeRefs) != 0 {
		project.IncludeReferences = includeRefs
	}

	if !opts.SkipNormalization {
		err := Normalize(project)
		if err != nil {
			return nil, err
		}
	}

	if opts.ConvertWindowsPaths {
		for i, service := range project.Services {
			for j, volume := range service.Volumes {
				service.Volumes[j] = convertVolumePath(volume)
			}
			project.Services[i] = service
		}
	}

	if !opts.SkipConsistencyCheck {
		err := checkConsistency(project)
		if err != nil {
			return nil, err
		}
	}

	project.ApplyProfiles(opts.Profiles)

	if !opts.SkipResolveEnvironment {
		err := project.ResolveServicesEnvironment(opts.discardEnvFiles)
		if err != nil {
			return nil, err
		}
	}

	return project, nil
}

func InvalidProjectNameErr(v string) error {
	return fmt.Errorf(
		"invalid project name %q: must consist only of lowercase alphanumeric characters, hyphens, and underscores as well as start with a letter or number",
		v,
	)
}

// projectName determines the canonical name to use for the project considering
// the loader Options as well as `name` fields in Compose YAML fields (which
// also support interpolation).
//
// TODO(milas): restructure loading so that we don't need to re-parse the YAML
// here, as it's both wasteful and makes this code error-prone.
func projectName(details types.ConfigDetails, opts *Options) (string, error) {
	projectName, projectNameImperativelySet := opts.GetProjectName()

	// if user did NOT provide a name explicitly, then see if one is defined
	// in any of the config files
	if !projectNameImperativelySet {
		var pjNameFromConfigFile string
		for _, configFile := range details.ConfigFiles {
			yml, err := ParseYAML(configFile.Content)
			if err != nil {
				// HACK: the way that loading is currently structured, this is
				// a duplicative parse just for the `name`. if it fails, we
				// give up but don't return the error, knowing that it'll get
				// caught downstream for us
				return "", nil
			}
			if val, ok := yml["name"]; ok && val != "" {
				sVal, ok := val.(string)
				if !ok {
					// HACK: see above - this is a temporary parsed version
					// that hasn't been schema-validated, but we don't want
					// to be the ones to actually report that, so give up,
					// knowing that it'll get caught downstream for us
					return "", nil
				}
				pjNameFromConfigFile = sVal
			}
		}
		if !opts.SkipInterpolation {
			interpolated, err := interp.Interpolate(
				map[string]interface{}{"name": pjNameFromConfigFile},
				*opts.Interpolate,
			)
			if err != nil {
				return "", err
			}
			pjNameFromConfigFile = interpolated["name"].(string)
		}
		pjNameFromConfigFile = NormalizeProjectName(pjNameFromConfigFile)
		if pjNameFromConfigFile != "" {
			projectName = pjNameFromConfigFile
		}
	}

	if projectName == "" {
		return "", errors.New("project name must not be empty")
	}

	if NormalizeProjectName(projectName) != projectName {
		return "", InvalidProjectNameErr(projectName)
	}

	return projectName, nil
}

func NormalizeProjectName(s string) string {
	r := regexp.MustCompile("[a-z0-9_-]")
	s = strings.ToLower(s)
	s = strings.Join(r.FindAllString(s, -1), "")
	return strings.TrimLeft(s, "_-")
}

var userDefinedKeys = []tree.Path{
	"services",
	"volumes",
	"networks",
	"secrets",
	"configs",
}

func groupXFieldsIntoExtensions(dict map[string]interface{}, p tree.Path) map[string]interface{} {
	extras := map[string]interface{}{}
	for key, value := range dict {
		skip := false
		for _, uk := range userDefinedKeys {
			if uk.Matches(p) {
				skip = true
				break
			}
		}
		if !skip && strings.HasPrefix(key, "x-") {
			extras[key] = value
			delete(dict, key)
			continue
		}
		switch v := value.(type) {
		case map[string]interface{}:
			dict[key] = groupXFieldsIntoExtensions(v, p.Next(key))
		case []interface{}:
			for i, e := range v {
				if m, ok := e.(map[string]interface{}); ok {
					v[i] = groupXFieldsIntoExtensions(m, p.Next(strconv.Itoa(i)))
				}
			}
		}
	}
	if len(extras) > 0 {
		dict[consts.Extensions] = extras
	}
	return dict
}

// Transform converts the source into the target struct with compose types transformer
// and the specified transformers if any.
func Transform(source interface{}, target interface{}) error {
	data := mapstructure.Metadata{}
	config := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			nameServices,
			decoderHook,
			cast),
		Result:   target,
		TagName:  "yaml",
		Metadata: &data,
	}
	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}
	return decoder.Decode(source)
}

// nameServices create implicit `name` key for convenience accessing service
func nameServices(from reflect.Value, to reflect.Value) (interface{}, error) {
	if to.Type() == reflect.TypeOf(types.Services{}) {
		nameK := reflect.ValueOf("name")
		iter := from.MapRange()
		for iter.Next() {
			name := iter.Key()
			elem := iter.Value()
			elem.Elem().SetMapIndex(nameK, name)
		}
	}
	return from.Interface(), nil
}

// keys need to be converted to strings for jsonschema
func convertToStringKeysRecursive(value interface{}, keyPrefix string) (interface{}, error) {
	if mapping, ok := value.(map[string]interface{}); ok {
		for key, entry := range mapping {
			var newKeyPrefix string
			if keyPrefix == "" {
				newKeyPrefix = key
			} else {
				newKeyPrefix = fmt.Sprintf("%s.%s", keyPrefix, key)
			}
			convertedEntry, err := convertToStringKeysRecursive(entry, newKeyPrefix)
			if err != nil {
				return nil, err
			}
			mapping[key] = convertedEntry
		}
		return mapping, nil
	}
	if mapping, ok := value.(map[interface{}]interface{}); ok {
		dict := make(map[string]interface{})
		for key, entry := range mapping {
			str, ok := key.(string)
			if !ok {
				return nil, formatInvalidKeyError(keyPrefix, key)
			}
			var newKeyPrefix string
			if keyPrefix == "" {
				newKeyPrefix = str
			} else {
				newKeyPrefix = fmt.Sprintf("%s.%s", keyPrefix, str)
			}
			convertedEntry, err := convertToStringKeysRecursive(entry, newKeyPrefix)
			if err != nil {
				return nil, err
			}
			dict[str] = convertedEntry
		}
		return dict, nil
	}
	if list, ok := value.([]interface{}); ok {
		var convertedList []interface{}
		for index, entry := range list {
			newKeyPrefix := fmt.Sprintf("%s[%d]", keyPrefix, index)
			convertedEntry, err := convertToStringKeysRecursive(entry, newKeyPrefix)
			if err != nil {
				return nil, err
			}
			convertedList = append(convertedList, convertedEntry)
		}
		return convertedList, nil
	}
	return value, nil
}

func formatInvalidKeyError(keyPrefix string, key interface{}) error {
	var location string
	if keyPrefix == "" {
		location = "at top level"
	} else {
		location = fmt.Sprintf("in %s", keyPrefix)
	}
	return errors.Errorf("Non-string key %s: %#v", location, key)
}

// Windows path, c:\\my\\path\\shiny, need to be changed to be compatible with
// the Engine. Volume path are expected to be linux style /c/my/path/shiny/
func convertVolumePath(volume types.ServiceVolumeConfig) types.ServiceVolumeConfig {
	volumeName := strings.ToLower(filepath.VolumeName(volume.Source))
	if len(volumeName) != 2 {
		return volume
	}

	convertedSource := fmt.Sprintf("/%c%s", volumeName[0], volume.Source[len(volumeName):])
	convertedSource = strings.ReplaceAll(convertedSource, "\\", "/")

	volume.Source = convertedSource
	return volume
}
