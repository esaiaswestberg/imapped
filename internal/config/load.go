package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	env "github.com/caarlos0/env/v11"
)

// Source records where an effective value came from.
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
)

// Field describes one configuration knob and where its value came from. The
// /settings page renders these, which turns "why didn't my env var take effect"
// from a debugging session into a glance.
type Field struct {
	TOMLPath string // e.g. "sync.body_batch_bytes"
	EnvVar   string // the env var that won, or the primary one if unset
	Value    string // rendered value, redacted for secrets
	Source   Source
	Secret   bool
}

// Result is a loaded configuration plus the provenance of every field.
type Result struct {
	Config     Config
	Fields     []Field
	FilePath   string // empty when no file was loaded
	FileLoaded bool
}

// secretTOMLPaths are rendered as a fingerprint rather than a value.
var secretTOMLPaths = map[string]bool{
	"encryption_master_key":        true,
	"db.url":                       true,
	"storage.s3_secret_access_key": true,
	"storage.s3_access_key_id":     true,
	"bootstrap.password":           true,
}

// DefaultFilePaths are probed in order when no explicit path is given.
var DefaultFilePaths = []string{"./imapped.toml", "/etc/imapped/config.toml"}

// envAliases maps a canonical environment variable to deprecated names that
// still work. The R2_* names predate generalising the blob store to any
// S3-compatible endpoint; existing deployments set them, so they keep working.
// The canonical name always wins when both are present.
var envAliases = map[string][]string{
	"S3_ENDPOINT":          {"R2_ENDPOINT"},
	"S3_BUCKET":            {"R2_BUCKET"},
	"S3_ACCESS_KEY_ID":     {"R2_ACCESS_KEY_ID"},
	"S3_SECRET_ACCESS_KEY": {"R2_SECRET_ACCESS_KEY"},
	"S3_REGION":            {"R2_REGION"},
}

// buildEnvironment snapshots the process environment and resolves deprecated
// aliases onto their canonical names. Returning a map rather than calling
// os.Setenv keeps Load free of global side effects, which matters because tests
// run in parallel against the same process environment.
func buildEnvironment() (env map[string]string, aliasUsed map[string]string) {
	env = make(map[string]string)
	for _, entry := range os.Environ() {
		if key, value, ok := strings.Cut(entry, "="); ok {
			env[key] = value
		}
	}
	aliasUsed = make(map[string]string)
	for canonical, legacy := range envAliases {
		if _, ok := env[canonical]; ok {
			continue // an explicit canonical value always wins
		}
		for _, name := range legacy {
			if value, ok := env[name]; ok {
				env[canonical] = value
				aliasUsed[canonical] = name
				break
			}
		}
	}
	return env, aliasUsed
}

// Load builds the effective configuration: defaults, then the TOML file if one
// exists, then the environment. Precedence is env > file > default.
//
// An explicit path that does not exist is an error; a probed default path that
// does not exist is skipped. Unknown keys in the file are an error, because a
// silently-ignored typo is a configuration system that lies to you.
func Load(explicitPath string) (*Result, error) {
	cfg := Default()
	res := &Result{}

	path, mustExist := explicitPath, explicitPath != ""
	if path == "" {
		if envPath := os.Getenv("IMAPPED_CONFIG"); envPath != "" {
			path, mustExist = envPath, true
		}
	}

	var fileKeys map[string]bool
	if path == "" {
		for _, candidate := range DefaultFilePaths {
			if _, err := os.Stat(candidate); err == nil {
				path = candidate
				break
			}
		}
	}

	if path != "" {
		keys, err := decodeFile(path, &cfg)
		switch {
		case err != nil && os.IsNotExist(err) && !mustExist:
			// Probed path vanished between Stat and Open; treat as absent.
		case err != nil:
			return nil, err
		default:
			fileKeys = keys
			res.FilePath = path
			res.FileLoaded = true
		}
	}

	environ, aliasUsed := buildEnvironment()
	envSet := envVarsPresent(&cfg, environ, aliasUsed)
	if err := env.ParseWithOptions(&cfg, env.Options{Environment: environ}); err != nil {
		return nil, fmt.Errorf("reading configuration from environment: %w", err)
	}

	res.Config = cfg
	res.Fields = provenance(&cfg, fileKeys, envSet)
	return res, nil
}

// decodeFile reads a TOML file into cfg and returns the set of keys it defined.
func decodeFile(path string, cfg *Config) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta, err := toml.Decode(string(data), cfg)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			names = append(names, key.String())
		}
		sort.Strings(names)
		return nil, fmt.Errorf("parsing %s: unknown configuration %s: %s",
			path, plural(len(names), "key", "keys"), strings.Join(names, ", "))
	}
	keys := make(map[string]bool, len(meta.Keys()))
	for _, key := range meta.Keys() {
		keys[key.String()] = true
	}
	return keys, nil
}

// walkFields visits every leaf configuration field, passing the dotted TOML path
// and the env var names declared for it.
func walkFields(cfg *Config, visit func(tomlPath string, envVars []string, value reflect.Value)) {
	var walk func(v reflect.Value, prefix string)
	walk = func(v reflect.Value, prefix string) {
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			tomlTag := field.Tag.Get("toml")
			if tomlTag == "" || tomlTag == "-" {
				continue
			}
			name := strings.Split(tomlTag, ",")[0]
			path := name
			if prefix != "" {
				path = prefix + "." + name
			}
			fv := v.Field(i)
			// Nested config sections are plain structs; leaf types like Duration
			// and ByteSize are named scalars, so recurse only on tagless structs.
			if fv.Kind() == reflect.Struct && field.Tag.Get("env") == "" {
				walk(fv, path)
				continue
			}
			var envVars []string
			if envTag := field.Tag.Get("env"); envTag != "" {
				for _, part := range strings.Split(envTag, ",") {
					part = strings.TrimSpace(part)
					// Skip caarlos0/env option words such as "required".
					if part == "" || strings.ToUpper(part) != part {
						continue
					}
					envVars = append(envVars, part)
				}
			}
			visit(path, envVars, fv)
		}
	}
	walk(reflect.ValueOf(cfg).Elem(), "")
}

// envVarsPresent records which fields have an environment value, captured
// before parsing so the final value can be attributed correctly. When a value
// arrived through a deprecated alias, the alias is reported so the settings page
// shows the variable the operator actually set.
func envVarsPresent(cfg *Config, environ, aliasUsed map[string]string) map[string]string {
	present := make(map[string]string)
	walkFields(cfg, func(path string, envVars []string, _ reflect.Value) {
		for _, name := range envVars {
			if _, ok := environ[name]; !ok {
				continue
			}
			if alias, ok := aliasUsed[name]; ok {
				present[path] = alias
			} else {
				present[path] = name
			}
			return
		}
	})
	return present
}

func provenance(cfg *Config, fileKeys map[string]bool, envSet map[string]string) []Field {
	var fields []Field
	walkFields(cfg, func(path string, envVars []string, value reflect.Value) {
		source := SourceDefault
		envVar := ""
		if len(envVars) > 0 {
			envVar = envVars[0]
		}
		if fileKeys[path] {
			source = SourceFile
		}
		if name, ok := envSet[path]; ok {
			source = SourceEnv
			envVar = name
		}
		secret := secretTOMLPaths[path]
		fields = append(fields, Field{
			TOMLPath: path,
			EnvVar:   envVar,
			Value:    renderValue(value, secret),
			Source:   source,
			Secret:   secret,
		})
	})
	return fields
}

func renderValue(v reflect.Value, secret bool) string {
	str := fmt.Sprintf("%v", v.Interface())
	if stringer, ok := v.Interface().(fmt.Stringer); ok {
		str = stringer.String()
	}
	if !secret || str == "" {
		return str
	}
	return Fingerprint(str)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// MustLoad is Load followed by Validate, for callers that should die on bad config.
func MustLoad(explicitPath string) (*Result, error) {
	res, err := Load(explicitPath)
	if err != nil {
		return nil, err
	}
	if err := res.Config.Validate(); err != nil {
		return nil, err
	}
	return res, nil
}

var errNoDatabase = errors.New("db.url (DATABASE_URL) is required")
