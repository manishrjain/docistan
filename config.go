package main

// One flat config file for what prod needs, so a deployment is a mount rather
// than a command line. The format is `key = value` with # comments — parsed
// here in twenty lines, because ten flat keys of URLs, ids and numbers need no
// sections, no quoting and no library. Keys mirror the flags (dashes become
// underscores) and are applied through flag.Set, so every flag is a config key
// automatically and the flag package's own parsing supplies the type errors.
// That includes the secret-valued ones — one mechanism, uniformly. The place
// for a secret is this file (or the environment), not argv, because argv is
// readable by every user on the machine through ps and by `docker inspect`
// for the life of the container; the flag help says so, and the choice is the
// operator's.

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// configEntry keeps its line number so an error names the place to fix, and
// file order so application is deterministic.
type configEntry struct {
	Key, Value string
	Line       int
}

func parseConfig(data string) ([]configEntry, error) {
	var out []configEntry
	seen := map[string]int{}
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: %q is not key = value", i+1, line)
		}
		key = strings.TrimSpace(key)
		// A trailing comment: # ends the value when it opens it or follows
		// whitespace. Glued to text it is part of the value, so the rare
		// secret containing one survives — write it flush against the text.
		if cut := trailingComment(value); cut >= 0 {
			value = value[:cut]
		}
		// A duplicate is a file arguing with itself. Taking either value
		// would mean the other line sits there looking load-bearing.
		if prev, dup := seen[key]; dup {
			return nil, fmt.Errorf("line %d: %s already set on line %d", i+1, key, prev)
		}
		seen[key] = i + 1
		out = append(out, configEntry{Key: key, Value: strings.TrimSpace(value), Line: i + 1})
	}
	return out, nil
}

// trailingComment finds where a value's comment begins, or -1. Split from the
// loop so the rule is testable by itself: it is exactly the rule a reader
// assumes, and the README's own example leans on it.
func trailingComment(value string) int {
	for i, r := range value {
		if r != '#' {
			continue
		}
		if i == 0 || value[i-1] == ' ' || value[i-1] == '\t' {
			return i
		}
	}
	return -1
}

// configFile resolves which file to read. An explicit -config replaces the
// list rather than extending it — same contract as keyFiles: naming a file is
// answering the question. The defaults may simply not exist, which is not an
// error; a named file that does not exist is one.
func configFile(flagValue string) (path string, required bool) {
	if flagValue != "" {
		return flagValue, true
	}
	var candidates []string
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "docovia", "config"))
	}
	candidates = append(candidates, "/etc/docovia/config")
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, false
		}
	}
	return "", false
}

// applyConfigFile reads the config and pushes its values into the flags that
// were not set on the command line, so the precedence is flag > config >
// built-in default with no per-setting code. Must run after flag.Parse and
// before anything reads the config struct.
func applyConfigFile(flagValue string) error {
	path, required := configFile(flagValue)
	if path == "" {
		return nil // no config file is the ordinary dev case
	}
	return applyConfig(flag.CommandLine, path, required)
}

// applyConfig is the testable core: the flag set is a parameter because the
// real one belongs to the process, and a test binary's belongs to go test.
func applyConfig(fs *flag.FlagSet, path string, required bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// A warning, not a refusal: the file plausibly holds keys, but whether
	// other accounts on this machine matter is the operator's question.
	if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o004 != 0 {
		logf("%s is world-readable and may hold secrets — consider chmod 600", path)
	}

	entries, err := parseConfig(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	for _, e := range entries {
		// The one key that cannot mean anything: by the time this file is
		// being read, which file to read has already been answered.
		if e.Key == "config" {
			return fmt.Errorf("%s line %d: a config file cannot name the config file", path, e.Line)
		}
		name := strings.ReplaceAll(e.Key, "_", "-")
		if fs.Lookup(name) == nil {
			// Refused rather than skipped: a typo'd key silently ignored
			// presents as its feature being mysteriously off.
			return fmt.Errorf("%s line %d: unknown key %q", path, e.Line, e.Key)
		}
		if set[name] {
			continue // the command line already answered this one
		}
		if err := fs.Set(name, e.Value); err != nil {
			return fmt.Errorf("%s line %d: %s = %q: %v", path, e.Line, e.Key, e.Value, err)
		}
	}
	logf("config loaded from %s", path)
	return nil
}
