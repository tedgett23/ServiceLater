package apps

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// LocalRoot is the directory holding this instance's local layer for every app, from SL_APPS_LOCAL_DIR.
// Each app's overrides live in <root>/<app-name>/, laid out like the app's own local/ folder:
//
//	<root>/incident-management/lifecycles/incident.yaml
//
// It is kept outside the app and the binary, on a volume, so upgrades replace the shipped layer and never touch it.
func LocalRoot() string { return os.Getenv("SL_APPS_LOCAL_DIR") }

func (b *Bundle) appName() string {
	var m struct {
		Name string `yaml:"name"`
	}
	if yaml.Unmarshal(b.Files["app.yaml"], &m) != nil || !nameRE.MatchString(m.Name) {
		return "" // Compile reports a bad manifest; here it just means there is nothing to look up
	}
	return m.Name
}

// AttachInstanceLocal reads this instance's overrides for the app from root. A missing directory means no
// overrides, which is the normal case.
func (b *Bundle) AttachInstanceLocal(root string) error {
	name := b.appName()
	if root == "" || name == "" {
		return nil
	}
	dir := filepath.Join(root, name)
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s should be a directory of local overrides", dir)
	}
	files, err := readFiles(os.DirFS(dir))
	if err != nil {
		return fmt.Errorf("reading local overrides in %s: %w", dir, err)
	}
	b.Instance, b.InstanceRoot = files, dir
	return nil
}
