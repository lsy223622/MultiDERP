package configexample

import _ "embed"

//go:embed config.example.yaml
var exampleYAML []byte

// Content returns a copy of the repository's example configuration.
func Content() []byte {
	return append([]byte(nil), exampleYAML...)
}
