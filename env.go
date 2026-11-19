package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type dotenv struct {
	path   string
	lines  []string
	values map[string]string
}

func readEnv(path string) (*dotenv, error) {
	env := &dotenv{path: path, values: make(map[string]string)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return env, nil
	}
	if err != nil {
		return nil, err
	}
	env.lines = strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	for i, line := range env.lines {
		key, value, ok := envEntry(line)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(value, "\""):
			value, err = strconv.Unquote(value)
		case strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2:
			value = value[1 : len(value)-1]
		default:
			value, _, _ = strings.Cut(value, " #")
			value = strings.TrimSpace(value)
		}
		if err != nil {
			return nil, fmt.Errorf("%s:%d: invalid quoted value", path, i+1)
		}
		env.values[key] = value
	}
	return env, nil
}

func envEntry(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "#") {
		return "", "", false
	}
	key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
	return strings.TrimSpace(key), strings.TrimSpace(value), ok
}

// Configuration is read at startup, before OAuth can update the token entries.
func (e *dotenv) config(key string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return e.values[key]
}

func (e *dotenv) save(values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(e.lines)+len(keys))
	for _, line := range e.lines {
		key, _, _ := envEntry(line)
		if _, replace := values[key]; !replace {
			lines = append(lines, line)
		}
	}
	for _, key := range keys {
		lines = append(lines, key+"="+strconv.Quote(values[key]))
	}
	file, err := os.CreateTemp(filepath.Dir(e.path), ".env-write-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), e.path); err != nil {
		return err
	}
	e.lines = lines
	for key, value := range values {
		e.values[key] = value
	}
	return nil
}
