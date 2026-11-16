package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"qswitch/internal/adapter"
	"qswitch/internal/buildinfo"
	"qswitch/internal/livefile"
)

type InstanceInfo struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Version      string         `json:"version"`
	OS           string         `json:"os"`
	ManagedTools []adapter.Tool `json:"managed_tools"`
}

// Environment describes a navigation destination, never a credential or proxy.
type Environment struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

func persistentInstanceID(dir string) (string, error) {
	lock, err := livefile.Acquire(filepath.Join(dir, "locks", "instance.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	path := filepath.Join(dir, "instance-id")
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		decoded, e := hex.DecodeString(id)
		if e != nil || len(decoded) != 16 {
			return "", errors.New("invalid instance-id; refusing to replace an existing identity")
		}
		return id, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(random[:])
	return id, livefile.AtomicWrite(path, []byte(id+"\n"), 0o600)
}

func (a *App) Instance() InstanceInfo {
	if a == nil {
		return InstanceInfo{Version: buildinfo.Version, OS: runtime.GOOS, ManagedTools: []adapter.Tool{}}
	}
	name := strings.TrimSpace(a.cfgSnapshot().Instance.Name)
	if name == "" {
		name, _ = os.Hostname()
	}
	if name == "" {
		name = "qswitch"
	}
	return InstanceInfo{ID: a.instanceID, Name: name, Version: buildinfo.Version, OS: runtime.GOOS, ManagedTools: a.ManagedTools()}
}

func (a *App) SetManagedTools(tools []adapter.Tool) error {
	if len(tools) == 0 {
		return errors.New("managed tool scope cannot be empty")
	}
	seen := map[adapter.Tool]bool{}
	copyTools := make([]adapter.Tool, 0, len(tools))
	for _, tool := range tools {
		if _, err := adapter.ParseTool(string(tool)); err != nil {
			return err
		}
		if seen[tool] {
			return fmt.Errorf("duplicate managed tool %s", tool)
		}
		seen[tool] = true
		copyTools = append(copyTools, tool)
	}
	a.scopeMu.Lock()
	a.managedTools = copyTools
	a.scopeMu.Unlock()
	return nil
}

func (a *App) ManagedTools() []adapter.Tool {
	a.scopeMu.RLock()
	defer a.scopeMu.RUnlock()
	if a.managedTools == nil {
		return adapter.AllTools()
	}
	return append([]adapter.Tool(nil), a.managedTools...)
}

func (a *App) CheckScope(tool adapter.Tool) error {
	if _, err := adapter.ParseTool(string(tool)); err != nil {
		return err
	}
	for _, t := range a.ManagedTools() {
		if t == tool {
			return nil
		}
	}
	return fmt.Errorf("%s 不在当前实例的管理范围内", tool)
}

func (a *App) Environments() ([]Environment, error) {
	out := []Environment{}
	b, err := os.ReadFile(filepath.Join(a.DataDir, "environments.json"))
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, errors.New("invalid environments directory")
	}
	if err := validateEnvironments(out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []Environment{}
	}
	return out, nil
}

func validateEnvironments(items []Environment) error {
	if len(items) > 64 {
		return errors.New("at most 64 environments are supported")
	}
	ids, urls := map[string]bool{}, map[string]bool{}
	for _, item := range items {
		if strings.TrimSpace(item.ID) != item.ID || item.ID == "" || len(item.ID) > 128 || strings.ContainsAny(item.ID, "\r\n\t /?#") {
			return errors.New("environment requires a valid instance id")
		}
		if strings.TrimSpace(item.Name) != item.Name || item.Name == "" || len(item.Name) > 128 || strings.ContainsAny(item.Name, "\r\n\t") {
			return errors.New("environment requires a name")
		}
		u, err := url.Parse(item.URL)
		if err != nil || len(item.URL) > 2048 || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return errors.New("environment URL must be an absolute http(s) URL without credentials, query or fragment")
		}
		if ids[item.ID] || urls[item.URL] {
			return errors.New("duplicate environment id or URL")
		}
		ids[item.ID], urls[item.URL] = true, true
	}
	return nil
}

func (a *App) SetEnvironments(items []Environment) error {
	if err := validateEnvironments(items); err != nil {
		return err
	}
	if items == nil {
		items = []Environment{}
	}
	b, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	lock, err := livefile.Acquire(filepath.Join(a.DataDir, "locks", "environments.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	return livefile.AtomicWrite(filepath.Join(a.DataDir, "environments.json"), append(b, '\n'), 0o600)
}

func containsTool(tools []adapter.Tool, want adapter.Tool) bool {
	for _, tool := range tools {
		if tool == want {
			return true
		}
	}
	return false
}
