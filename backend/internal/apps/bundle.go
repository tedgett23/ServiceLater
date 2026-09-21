package apps

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

// Limits protect the installer from a hostile or broken bundle.
const (
	maxFiles     = 2000
	maxFileBytes = 2 << 20  // 2 MiB per file
	maxBytes     = 16 << 20 // 16 MiB per bundle
)

// Bundle is an app as files, before it is interpreted.
//
// An app has two layers of definitions. default/ is what the app ships, and upgrades replace it. local/ is an
// operator's own changes, which upgrades never touch, and which override default/ (see mergeYAML). For an app
// that ships inside the binary the local layer cannot sit beside it, so it lives on disk under the instance's
// local directory instead (Instance).
type Bundle struct {
	// Files are the app folder's contents, keyed by slash-separated path: app.yaml, README.md, default/... and,
	// when installing from a folder, local/...
	Files map[string][]byte
	// Instance is the local layer kept outside the app for this instance, keyed like the contents of local/
	// (for example "lifecycles/incident.yaml"). It overrides both default/ and local/.
	Instance     map[string][]byte
	InstanceRoot string // where Instance was read from, for messages
}

// Definition is one validated piece of an app that gets stored: a lifecycle now, rules and workflows later.
type Definition struct {
	Kind      string
	Name      string
	File      string
	Layers    []string // which layers contributed: default, local, instance
	Body      json.RawMessage
	Checksum  string
	Lifecycle *Lifecycle
}

// Source is the layers joined for storage and display, for example "default+local".
func (d Definition) Source() string { return strings.Join(d.Layers, "+") }

// Compiled is a bundle that passed validation.
type Compiled struct {
	Manifest    Manifest
	Definitions []Definition
}

// definition folders: what each holds, and whether this version can install it.
var definitionDirs = map[string]struct{ kind, unsupported string }{
	"lifecycles":    {kind: "lifecycle"},
	"rules":         {unsupported: "rules are not supported yet"},
	"workflows":     {unsupported: "workflows are not supported yet"},
	"forms":         {unsupported: "forms are not supported yet"},
	"catalog":       {unsupported: "catalog items are not supported yet"},
	"knowledge":     {unsupported: "knowledge articles are not supported yet"},
	"notifications": {unsupported: "notification templates are not supported yet"},
	"roles":         {unsupported: "roles are not supported yet"},
	"ui":            {unsupported: "declarative views are not supported yet"},
}

// Load reads a bundle from a folder or a .swapp file.
func Load(p string) (*Bundle, error) {
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if st.IsDir() {
		files, err := readFiles(os.DirFS(p))
		return &Bundle{Files: files}, err
	}
	return loadArchive(p)
}

func hidden(name string) bool { return strings.HasPrefix(name, ".") }

// readFiles reads every visible file under fsys, refusing symlinks and enforcing size limits.
func readFiles(fsys fs.FS) (map[string][]byte, error) {
	files := map[string][]byte{}
	var total int64
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if hidden(d.Name()) { // .gitkeep, .DS_Store, .git and friends
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link, which apps may not contain", p)
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return add(files, p, data, &total)
	})
	return files, err
}

func add(files map[string][]byte, name string, data []byte, total *int64) error {
	*total += int64(len(data))
	switch {
	case len(files) >= maxFiles:
		return fmt.Errorf("the app has more than %d files", maxFiles)
	case len(data) > maxFileBytes:
		return fmt.Errorf("%s is larger than %d bytes", name, maxFileBytes)
	case *total > maxBytes:
		return fmt.Errorf("the app is larger than %d bytes", maxBytes)
	}
	files[name] = data
	return nil
}

func loadArchive(file string) (*Bundle, error) {
	zr, err := zip.OpenReader(file)
	if err != nil {
		return nil, fmt.Errorf("%s is not a folder or a valid .swapp archive: %w", file, err)
	}
	defer func() { _ = zr.Close() }()
	files := map[string][]byte{}
	var total int64
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(strings.ReplaceAll(f.Name, "\\", "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("archive entry %q escapes the app folder", f.Name)
		}
		if anyHidden(name) {
			continue
		}
		if f.UncompressedSize64 > maxFileBytes {
			return nil, fmt.Errorf("%s is larger than %d bytes", name, maxFileBytes)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxFileBytes+1))
		_ = rc.Close()
		if err != nil {
			return nil, err
		}
		if err := add(files, name, data, &total); err != nil {
			return nil, err
		}
	}
	return &Bundle{Files: files}, nil
}

func anyHidden(name string) bool {
	for _, part := range strings.Split(name, "/") {
		if hidden(part) {
			return true
		}
	}
	return false
}

// Shipped returns the bundle without any local layer: what an app distributes and what an upgrade replaces.
func (b *Bundle) Shipped() *Bundle {
	out := &Bundle{Files: map[string][]byte{}}
	for name, data := range b.Files {
		if name == "app.yaml" || name == "README.md" || strings.HasPrefix(name, "default/") {
			out.Files[name] = data
		}
	}
	return out
}

// HasLocal reports whether the bundle carries any local overrides.
func (b *Bundle) HasLocal() bool {
	if len(b.Instance) > 0 {
		return true
	}
	for name := range b.Files {
		if strings.HasPrefix(name, "local/") {
			return true
		}
	}
	return false
}

// layer is one set of definition files, keyed relative to its root (for example "lifecycles/incident.yaml").
type layer struct {
	name    string // default, local or instance
	display func(rel string) string
	files   map[string][]byte
}

func (b *Bundle) layers(p *Problems) []layer {
	def := layer{name: "default", display: func(r string) string { return "default/" + r }, files: map[string][]byte{}}
	loc := layer{name: "local", display: func(r string) string { return "local/" + r }, files: map[string][]byte{}}
	for name, data := range b.Files {
		dir, rest, nested := strings.Cut(name, "/")
		switch {
		case !nested:
			if name != "app.yaml" && name != "README.md" {
				p.add(name, "unexpected file at the top of the app (only app.yaml and README.md belong there)")
			}
		case dir == "default":
			def.files[rest] = data
		case dir == "local":
			loc.files[rest] = data
		default:
			p.add(name, "%q is not a valid folder: shipped definitions go in default/ and local changes go in local/", dir)
		}
	}
	out := []layer{def, loc}
	if len(b.Instance) > 0 {
		root := b.InstanceRoot
		out = append(out, layer{name: "instance", display: func(r string) string { return path.Join(root, r) }, files: b.Instance})
	}
	return out
}

// Compile validates a bundle and returns its definitions, or every problem found. Local layers are merged over
// the shipped one before anything is parsed, so problems describe the effective result.
func (b *Bundle) Compile() (*Compiled, error) {
	var p Problems

	data, ok := b.Files["app.yaml"]
	if !ok {
		p.add("", "app.yaml is missing")
		return nil, p
	}
	m := parseManifest(data, &p)

	layers := b.layers(&p)
	valid := make([]map[string][]byte, len(layers))
	seenPath := map[string]bool{}
	for i, l := range layers {
		valid[i] = map[string][]byte{}
		names := make([]string, 0, len(l.files))
		for n := range l.files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, rel := range names {
			if checkDefinitionFile(l.display(rel), rel, &p) {
				valid[i][rel] = l.files[rel]
				seenPath[rel] = true
			}
		}
	}

	rels := make([]string, 0, len(seenPath))
	for rel := range seenPath {
		rels = append(rels, rel)
	}
	sort.Strings(rels)

	var defs []Definition
	seen := map[string]string{}
	for _, rel := range rels {
		var used []string
		var content []byte
		display := ""
		for i, l := range layers {
			over, has := valid[i][rel]
			if !has {
				continue
			}
			used = append(used, l.name)
			if content == nil {
				content, display = over, l.display(rel)
				continue
			}
			merged, err := mergeYAML(content, over)
			if err != nil {
				p.add(l.display(rel), "cannot apply this override to the shipped %s: %v", rel, err)
				content = nil
				break
			}
			content, display = merged, rel+" ("+strings.Join(used, " + ")+")"
		}
		if content == nil {
			continue
		}
		dir, _, _ := strings.Cut(rel, "/")
		if d := compileDefinition(definitionDirs[dir].kind, display, content, &p); d != nil {
			d.Layers = used
			key := d.Kind + "/" + d.Name
			if other, dup := seen[key]; dup {
				p.add(display, "defines %s %q, which %s already defines", d.Kind, d.Name, other)
			}
			seen[key] = display
			defs = append(defs, *d)
		}
	}
	if err := p.orNil(); err != nil || m == nil {
		return nil, p
	}
	return &Compiled{Manifest: *m, Definitions: defs}, nil
}

// checkDefinitionFile reports whether a file in a layer is a definition this version can install.
func checkDefinitionFile(display, rel string, p *Problems) bool {
	dir, rest, nested := strings.Cut(rel, "/")
	if !nested {
		p.add(display, "definitions belong in a folder such as lifecycles/, not directly here")
		return false
	}
	switch dir {
	case "migrations":
		p.add(display, "migrations are reserved and not supported in this version: apps are data only")
		return false
	case "scripts":
		p.add(display, "scripts are not supported: apps contain no code, use rules and workflows instead")
		return false
	}
	info, known := definitionDirs[dir]
	switch {
	case !known:
		p.add(display, "unknown folder %q", dir)
	case info.unsupported != "":
		p.add(display, "%s", info.unsupported)
	case !strings.HasSuffix(rest, ".yaml") && !strings.HasSuffix(rest, ".yml"):
		p.add(display, "only .yaml files belong in %s/", dir)
	default:
		return true
	}
	return false
}

func compileDefinition(kind, file string, data []byte, p *Problems) *Definition {
	switch kind {
	case "lifecycle":
		lc := parseLifecycle(file, data, p)
		if lc == nil {
			return nil
		}
		body, _ := json.Marshal(lc) // fields marshal in a fixed order, so the checksum is stable
		sum := sha256.Sum256(body)
		return &Definition{Kind: kind, Name: lc.RecordType, File: file, Body: body, Checksum: hex.EncodeToString(sum[:]), Lifecycle: lc}
	}
	return nil
}

// Validate loads a path and reports every problem in it. It needs no database.
// The shipped layer must be valid on its own, because that is what gets distributed; if there are local overrides,
// the combined result must be valid too.
func Validate(p string) (*Compiled, error) { return ValidateIn(p, "") }

// ValidateIn is Validate with the instance's local layer (see LocalRoot) attached as well.
func ValidateIn(p, localRoot string) (*Compiled, error) {
	b, err := Load(p)
	if err != nil {
		return nil, err
	}
	if err := b.AttachInstanceLocal(localRoot); err != nil {
		return nil, err
	}
	return b.validate()
}

func (b *Bundle) validate() (*Compiled, error) {
	shipped, err := b.Shipped().Compile()
	if err != nil {
		return nil, err
	}
	if !b.HasLocal() {
		return shipped, nil
	}
	full, err := b.Compile()
	if err != nil {
		var p Problems
		if errors.As(err, &p) {
			p = append(Problems{{Message: "the shipped app is valid, but with local overrides applied:"}}, p...)
			return nil, p
		}
		return nil, err
	}
	return full, nil
}

// Pack writes the shipped part of an app as a .swapp archive; a local/ folder is never included. Output is
// deterministic: sorted entries and fixed timestamps, so packing the same app twice gives identical bytes.
func Pack(p string) (name string, archive []byte, err error) {
	b, err := Load(p)
	if err != nil {
		return "", nil, err
	}
	shipped := b.Shipped()
	c, err := shipped.Compile()
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(shipped.Files))
	for n := range shipped.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	stamp := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate, Modified: stamp})
		if err != nil {
			return "", nil, err
		}
		if _, err := w.Write(shipped.Files[n]); err != nil {
			return "", nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("%s-%s.swapp", c.Manifest.Name, c.Manifest.Version), buf.Bytes(), nil
}
