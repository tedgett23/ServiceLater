package apps

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"

	"gopkg.in/yaml.v3"
)

// FormatVersion is the version of the app format this build understands (apiVersion in app.yaml).
const FormatVersion = 1

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{1,62}$`)

type Manifest struct {
	APIVersion  int          `yaml:"apiVersion" json:"apiVersion"`
	Name        string       `yaml:"name" json:"name"`
	Version     string       `yaml:"version" json:"version"`
	Author      string       `yaml:"author" json:"author"`
	Description string       `yaml:"description" json:"description"`
	MinVersion  string       `yaml:"ServiceLater_min_version" json:"ServiceLater_min_version"`
	Deps        []Dependency `yaml:"dependencies" json:"dependencies"`
	Permissions []string     `yaml:"permissions" json:"permissions"`
	Config      []ConfigItem `yaml:"config" json:"config"`
}

// Dependency is written in YAML as a one-entry map: `- core-itsm: ">=0.1.0"`.
type Dependency struct {
	Name       string `json:"name"`
	Constraint string `json:"constraint"`
}

func (d *Dependency) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 {
		return fmt.Errorf("line %d: a dependency is one entry like `core-itsm: \">=0.1.0\"`", n.Line)
	}
	d.Name = n.Content[0].Value
	d.Constraint = n.Content[1].Value
	return nil
}

func (d Dependency) MarshalYAML() (any, error) { return map[string]string{d.Name: d.Constraint}, nil }

type ConfigItem struct {
	Key   string `yaml:"key" json:"key"`
	Label string `yaml:"label" json:"label"`
	Type  string `yaml:"type" json:"type"` // string, secret, bool or int
}

// decodeStrict parses YAML and rejects unknown fields, so a typo like `discription:` is an error, not silence.
func decodeStrict(data []byte, into any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(into); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func parseManifest(data []byte, p *Problems) *Manifest {
	const file = "app.yaml"
	var m Manifest
	if err := decodeStrict(data, &m); err != nil {
		p.add(file, "%v", err)
		return nil
	}
	if m.APIVersion != FormatVersion {
		p.add(file, "apiVersion must be %d (this build understands only that format), got %d", FormatVersion, m.APIVersion)
	}
	if !nameRE.MatchString(m.Name) {
		p.add(file, "name %q must be 2-63 characters: lowercase letters, digits and hyphens, starting with a letter", m.Name)
	}
	if _, err := ParseVersion(m.Version); err != nil {
		p.add(file, "version: %v", err)
	}
	if m.Description == "" {
		p.add(file, "description is required")
	}
	if m.MinVersion != "" {
		if _, err := ParseVersion(m.MinVersion); err != nil {
			p.add(file, "ServiceLater_min_version: %v", err)
		}
	}
	seen := map[string]bool{}
	for _, d := range m.Deps {
		switch {
		case !nameRE.MatchString(d.Name):
			p.add(file, "dependency name %q is not a valid app name", d.Name)
		case d.Name == m.Name:
			p.add(file, "an app cannot depend on itself")
		case seen[d.Name]:
			p.add(file, "dependency %q is listed twice", d.Name)
		}
		seen[d.Name] = true
		if _, err := ParseConstraint(d.Constraint); err != nil {
			p.add(file, "dependency %s: %v", d.Name, err)
		}
	}
	for _, perm := range m.Permissions {
		if perm == "" || !regexp.MustCompile(`^[a-z_]+:\S+$`).MatchString(perm) {
			p.add(file, "permission %q must look like resource:action, e.g. incidents:update", perm)
		}
	}
	keys := map[string]bool{}
	for _, c := range m.Config {
		switch {
		case c.Key == "" || c.Label == "":
			p.add(file, "every config item needs a key and a label")
		case keys[c.Key]:
			p.add(file, "config key %q is listed twice", c.Key)
		case c.Type != "string" && c.Type != "secret" && c.Type != "bool" && c.Type != "int":
			p.add(file, "config %q: type must be string, secret, bool or int", c.Key)
		}
		keys[c.Key] = true
	}
	return &m
}
