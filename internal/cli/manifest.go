package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

// readManifests reads resources from YAML files. A file may hold several
// resources separated by "---". JSON files work too, since JSON is valid YAML.
// Each resource is returned as JSON, the format the API accepts.
func readManifests(paths []string) ([]json.RawMessage, error) {
	var resources []json.RawMessage
	for _, path := range paths {
		data, err := os.ReadFile(path) //nolint:gosec // reading the file the user named is the point
		if err != nil {
			return nil, err
		}
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for doc := 1; ; doc++ {
			var v any
			err := dec.Decode(&v)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, doc, err)
			}
			if v == nil {
				continue // an empty document, such as a trailing "---"
			}
			out, err := json.Marshal(v)
			if err != nil {
				return nil, fmt.Errorf("%s: document %d: %w", path, doc, err)
			}
			resources = append(resources, out)
		}
	}
	if len(resources) == 0 {
		return nil, errors.New("no resources found in the given files")
	}
	return resources, nil
}

// stringsFlag collects a flag given several times, such as -f a.yaml -f b.yaml.
type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint(*s) }

func (s *stringsFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}
