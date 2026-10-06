package builder

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func (b *Builder) downloadNewCommit(URL string, projectName string) error {

	resp, err := http.Get(URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	err = os.MkdirAll(b.downloadPath, 0755)
	if err != nil {
		return err
	}

	out, err := os.Create(filepath.Join(b.downloadPath, projectName+".zip"))
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		return err
	}

	return nil
}

func (b *Builder) unpackNewProject(projectName string) error {

	r, err := zip.OpenReader(filepath.Join(b.downloadPath, projectName+".zip"))
	if err != nil {
		return err
	}
	defer r.Close()

	for _, file := range r.File {
		filePath := filepath.Join(b.stagingPath, file.Name)

		// Check for zip slip (Check for malicious files)
		if !strings.HasPrefix(filePath, filepath.Clean(b.stagingPath)+string(os.PathSeparator)) {
			return os.ErrPermission
		}

		if file.FileInfo().IsDir() {
			err := os.MkdirAll(filePath, os.ModePerm)
			if err != nil {
				return err
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(filePath), os.ModePerm); err != nil {
			return err
		}

		rc, err := file.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		outFile, err := os.Create(filePath)
		if err != nil {
			rc.Close()
			return err
		}

		_, err = io.Copy(outFile, rc)
		outFile.Close()
		rc.Close()
		if err != nil {
			return err
		}

	}
	return nil
}

func (b *Builder) createContainer(projectName string) error {
	projectDir := filepath.Join(b.stagingPath, projectName+"-main")

	required, err := findComposeVars(projectDir)
	if err != nil {
		return fmt.Errorf("discover compose vars: %w", err)
	}

	keys := make([]string, 0, len(required))
	for v := range required {
		keys = append(keys, v)
	}

	secrets, err := b.cove.GetSecrets(keys...)
	if err != nil {
		return fmt.Errorf("fetch secrets for %s: %w", projectName, err)
	}

	env := os.Environ()
	for key, val := range secrets {
		env = append(env, fmt.Sprintf("%s=%s", key, val))
	}

	cmd := exec.Command("docker", "compose", "up", "-d", "--build", "--remove-orphans")
	cmd.Dir = projectDir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = env

	return cmd.Run()
}

func findComposeVars(dir string) (map[string]struct{}, error) {

	composeVarRx := regexp.MustCompile(`\$\{([^}:]+)(?::[^}]*)?\}`)

	cmd := exec.Command("docker", "compose", "config", "--no-interpolate")
	cmd.Dir = dir

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose config failed: %v\n%s", err, out.String())
	}

	matches := composeVarRx.FindAllStringSubmatch(out.String(), -1)
	vars := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		// m[1] is the variable name before any default/required modifiers
		name := m[1]
		// Very light sanity filter: environment variable-ish
		// (compose allows broader names technically, but most are A-Z0-9_)
		// You can drop this if you want everything.
		if name != "" {
			vars[name] = struct{}{}
		}
	}
	return vars, nil

}
