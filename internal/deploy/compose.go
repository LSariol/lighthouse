package deploy

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
)

// placeholder matches ${KEY} and ${KEY:...} in a compose file. Known to be
// wrong for ${KEY-default}, $${KEY} and $KEY (DOCUMENTATION.md B6); step 3
// asks Compose instead (docker compose config --variables).
var placeholder = regexp.MustCompile(`\$\{([^}:]+)(?::[^}]*)?\}`)

// composeUp fetches the secrets the project's compose file names and runs
// docker compose up with them in its environment. The project was unpacked
// to <staging>/<container>-main.
func (d *Deployer) composeUp(container string) error {
	dir := filepath.Join(d.stagingPath, container+"-main")

	keys, err := placeholders(dir)
	if err != nil {
		return err
	}

	secrets, err := d.cove.GetSecrets(keys...)
	if err != nil {
		return fmt.Errorf("fetch secrets for %s: %w", container, err)
	}

	env := os.Environ()
	for key, value := range secrets {
		env = append(env, key+"="+value)
	}

	cmd := exec.Command("docker", "compose", "up", "-d", "--build", "--remove-orphans")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = env
	return cmd.Run()
}

// placeholders returns the keys of every ${KEY} in the compose file in dir,
// sorted and without duplicates.
func placeholders(dir string) ([]string, error) {
	cmd := exec.Command("docker", "compose", "config", "--no-interpolate")
	cmd.Dir = dir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose config failed: %v\n%s", err, out.String())
	}

	return placeholderKeys(out.String()), nil
}

func placeholderKeys(config string) []string {
	seen := map[string]bool{}
	var keys []string
	for _, m := range placeholder.FindAllStringSubmatch(config, -1) {
		if key := m[1]; key != "" && !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}
