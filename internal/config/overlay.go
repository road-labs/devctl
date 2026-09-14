package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// OverlayName is the personal manifest: the committed one says what the
// repository needs, this one says how this machine provides it. It sits beside
// devctl.yaml, belongs in .gitignore, and is optional.
//
// It exists because the two are different questions. Everyone on a project
// needs a database; whether it arrives from Docker, a container runtime or a
// box under someone's desk is nobody else's business, and a committed manifest
// that picks one forces it on everyone.
const OverlayName = "devctl.mine.yaml"

// LoadWithOverlay reads the manifest and, if overlayPath exists, merges it over
// the top before validating. A missing overlay is not an error.
func LoadWithOverlay(path, overlayPath string) (*File, error) {
	base, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	overlay, err := os.ReadFile(overlayPath)
	switch {
	case os.IsNotExist(err):
		return parse(path, base)
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", overlayPath, err)
	}

	merged, err := mergeYAML(base, overlay)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", overlayPath, err)
	}
	file, err := parse(path, merged)
	if err != nil {
		// The overlay is the thing that changed, so name it: the committed
		// manifest on its own is presumably fine.
		return nil, fmt.Errorf("with %s applied: %w", overlayPath, err)
	}
	return file, nil
}

func parse(path string, raw []byte) (*File, error) {
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// mergeYAML applies overlay over base as plain data, before either becomes a
// File, so "present in the overlay" is decided by the document rather than by
// whether a Go field happens to be its zero value. Setting a port to 0 or a
// flag to false therefore means what it says.
func mergeYAML(base, overlay []byte) ([]byte, error) {
	var b, o any
	if err := yaml.Unmarshal(base, &b); err != nil {
		return nil, fmt.Errorf("parse the manifest: %w", err)
	}
	if err := yaml.Unmarshal(overlay, &o); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return yaml.Marshal(merge(b, o))
}

// merge returns overlay applied over base:
//
//   - mappings merge key by key, recursively;
//   - a sequence whose entries are mappings with a name merges by that name,
//     so one port can be changed without restating the service, and an entry
//     the base does not have is added;
//   - anything else the overlay replaces outright.
func merge(base, overlay any) any {
	bm, bok := base.(map[string]any)
	om, ook := overlay.(map[string]any)
	if bok && ook {
		out := make(map[string]any, len(bm)+len(om))
		for k, v := range bm {
			out[k] = v
		}
		for k, v := range om {
			if existing, found := out[k]; found {
				out[k] = merge(existing, v)
				continue
			}
			out[k] = v
		}
		return out
	}

	bs, bok := base.([]any)
	os_, ook := overlay.([]any)
	if bok && ook && named(bs) && named(os_) {
		out := make([]any, len(bs))
		copy(out, bs)
		for _, entry := range os_ {
			name := nameOf(entry)
			replaced := false
			for i, existing := range out {
				if nameOf(existing) == name {
					out[i] = merge(existing, entry)
					replaced = true
					break
				}
			}
			if !replaced {
				out = append(out, entry)
			}
		}
		return out
	}

	return overlay
}

// named reports whether every entry is a mapping carrying a non-empty name, the
// only case where merging a sequence by name is unambiguous.
func named(seq []any) bool {
	if len(seq) == 0 {
		return false
	}
	for _, entry := range seq {
		if nameOf(entry) == "" {
			return false
		}
	}
	return true
}

func nameOf(entry any) string {
	m, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	name, _ := m["name"].(string)
	return name
}
