// Package userconfig loads optional operator config for docs-puller (cwd profile
// roots, pin scan roots, tools-monorepo pin scopes, extra source keywords, and
// supplemental profile directories). OSS installs ship with empty defaults; set
// DOCS_PULLER_CONFIG or place config at ~/.docs-puller/config.yaml.
package userconfig

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/nstranquist/docs-puller/internal/apppaths"
	"gopkg.in/yaml.v3"
)

const configEnvVar = "DOCS_PULLER_CONFIG"

// Config is the optional operator config file schema.
type Config struct {
	CwdProfiles    []CwdProfileEntry    `yaml:"cwd_profiles"`
	PinScanRoots   []string             `yaml:"pin_scan_roots"`
	ToolsPinScopes []ToolsPinScopeEntry `yaml:"tools_pin_scopes"`
	SourceKeywords map[string][]string  `yaml:"source_keywords"`
	ProfilesDir    string               `yaml:"profiles_dir"`
	configFile     string               `yaml:"-"`
}

type CwdProfileEntry struct {
	Profile string   `yaml:"profile"`
	Roots   []string `yaml:"roots"`
}

type ToolsPinScopeEntry struct {
	PathContains string `yaml:"path_contains"`
	Basename     string `yaml:"basename"`
	Scope        string `yaml:"scope"`
}

var (
	loadOnce sync.Once
	cached   Config
	loadErr  error
)

// Load returns the effective config (empty when no file is present).
func Load() (Config, error) {
	loadOnce.Do(func() {
		cached, loadErr = loadFromDisk()
	})
	return cached, loadErr
}

// Reset clears the cache (tests only).
func Reset() {
	loadOnce = sync.Once{}
	cached = Config{}
	loadErr = nil
}

func loadFromDisk() (Config, error) {
	path, err := resolveConfigPath()
	if err != nil {
		return Config{}, err
	}
	if path == "" {
		return Config{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, nil
		}
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(body, &cfg); err != nil {
		return Config{}, err
	}
	cfg.configFile = path
	return cfg, nil
}

func resolveConfigPath() (string, error) {
	if v := strings.TrimSpace(os.Getenv(configEnvVar)); v != "" {
		return expandHome(v)
	}
	state, err := apppaths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "config.yaml"), nil
}

// ProfileSearchDirs returns supplemental profile directories in lookup order.
// Later dirs do not override earlier ones for LoadProfile — first hit wins.
func ProfileSearchDirs(out string) ([]string, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	var dirs []string
	if out != "" {
		dirs = append(dirs, filepath.Join(out, "profiles"))
	}
	state, err := apppaths.StateDir()
	if err != nil {
		return nil, err
	}
	dirs = append(dirs, filepath.Join(state, "profiles"))
	if cfg.ProfilesDir != "" {
		p := cfg.ProfilesDir
		if !filepath.IsAbs(p) {
			if cfg.configFile == "" {
				return dedupeDirs(dirs), nil
			}
			p = filepath.Join(filepath.Dir(cfg.configFile), p)
		} else if expanded, err := expandHome(p); err == nil {
			p = expanded
		} else {
			return nil, err
		}
		dirs = append(dirs, filepath.Clean(p))
	} else if cfg.configFile != "" {
		dirs = append(dirs, filepath.Dir(cfg.configFile))
	}
	return dedupeDirs(dirs), nil
}

// MatchCwdProfile returns the profile name when cwd sits under a configured root.
func MatchCwdProfile(cwd string) (profile string, ok bool) {
	if cwd == "" {
		return "", false
	}
	cfg, err := Load()
	if err != nil || len(cfg.CwdProfiles) == 0 {
		return "", false
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	for _, entry := range cfg.CwdProfiles {
		name := strings.TrimSpace(entry.Profile)
		if name == "" {
			continue
		}
		for _, root := range entry.Roots {
			rootAbs, err := expandHome(root)
			if err != nil {
				continue
			}
			rootAbs, err = filepath.Abs(rootAbs)
			if err != nil {
				continue
			}
			if abs == rootAbs || strings.HasPrefix(abs, rootAbs+string(filepath.Separator)) {
				return name, true
			}
		}
	}
	return "", false
}

// PinScanRoots returns configured pin scan roots that exist on disk.
func PinScanRoots() ([]string, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, root := range cfg.PinScanRoots {
		p, err := expandHome(root)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			out = append(out, p)
		}
	}
	return out, nil
}

// ClassifyToolsMonorepo maps a pin scan root to tools kind/scope when configured.
func ClassifyToolsMonorepo(root string) (kind, scope string, ok bool) {
	cfg, err := Load()
	if err != nil {
		return "", "", false
	}
	root = filepath.Clean(root)
	slash := filepath.ToSlash(root)
	base := filepath.Base(root)
	for _, rule := range cfg.ToolsPinScopes {
		scope = strings.TrimSpace(rule.Scope)
		if scope == "" {
			continue
		}
		if rule.Basename != "" && base == rule.Basename {
			return "tools", scope, true
		}
		if rule.PathContains != "" && strings.Contains(slash, rule.PathContains) {
			return "tools", scope, true
		}
	}
	return "", "", false
}

// ExtraSourceKeywords returns configured source keyword boosts.
func ExtraSourceKeywords() (map[string][]string, error) {
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	if len(cfg.SourceKeywords) == 0 {
		return nil, nil
	}
	out := make(map[string][]string, len(cfg.SourceKeywords))
	for src, kws := range cfg.SourceKeywords {
		out[src] = append([]string(nil), kws...)
	}
	return out, nil
}

func expandHome(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	if path[0] == '~' {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if len(path) == 1 || path[1] == '/' {
			return filepath.Join(home, path[1:]), nil
		}
	}
	return path, nil
}

func dedupeDirs(dirs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		d = filepath.Clean(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}
