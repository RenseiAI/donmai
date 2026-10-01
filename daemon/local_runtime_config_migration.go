package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/RenseiAI/donmai/agent"
	"gopkg.in/yaml.v3"
)

// PrepareLocalRuntimeConfig performs only the explicit, known-version startup
// upgrade. It never marshals a default-expanded Config or substitutes secrets.
// Readers do not call this function: startup/setup own its visible write.
func PrepareLocalRuntimeConfig(path string) (bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	parent, err := os.OpenRoot(filepath.Dir(absolute))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = parent.Close() }()
	raw, err := parent.ReadFile(filepath.Base(absolute))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read startup configuration: %w", err)
	}
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	if err = decoder.Decode(&document); err != nil {
		return false, err
	}
	var extra yaml.Node
	if err = decoder.Decode(&extra); err != io.EOF {
		return false, errors.New("startup configuration must contain one YAML document")
	}
	var original Config
	if err = document.Decode(&original); err != nil {
		return false, err
	}
	if !localRuntimeRequested(&original) {
		return false, nil
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return false, errors.New("local startup configuration must be a mapping")
	}
	root := document.Content[0]
	version := localYAMLMember(root, "apiVersion")
	if version == nil || version.Kind != yaml.ScalarNode || version.Tag != "!!str" || version.Anchor != "" {
		return false, &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnconfigured, Detail: "explicit, unaliased local configuration schema version is required"}
	}
	if version.Value == LocalRuntimeConfigAPIVersion {
		return false, ValidateLocalExecutionSecurity(&original)
	}
	if version.Value != "donmai.dev/v1" {
		return false, &agent.ExecutionSecurityError{Code: agent.ExecutionSecurityUnresolvable, Detail: "unknown local configuration schema version"}
	}
	if _, err = localQueueRoot(original.Orchestrator.URL); err != nil {
		return false, err
	}
	if err = validateConfig(&original); err != nil {
		return false, err
	}
	local := localYAMLMember(root, "localRuntime")
	if local == nil {
		local = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		root.Content = append(root.Content, localYAMLKey("localRuntime"), local)
	}
	if local.Kind != yaml.MappingNode || local.Anchor != "" {
		return false, errors.New("automatic local policy upgrade requires an unaliased localRuntime mapping; preserve it for explicit setup")
	}
	seeded := localYAMLMember(local, "executionSecurity") == nil
	if seeded {
		var policy yaml.Node
		if err = policy.Encode(InitialLocalExecutionSecurity()); err != nil {
			return false, err
		}
		local.Content = append(local.Content, localYAMLKey("executionSecurity"), &policy)
	} else if original.LocalRuntime == nil || original.LocalRuntime.ExecutionSecurity.Validate() != nil {
		return false, errors.New("existing local policy is malformed; it cannot be replaced by a seed")
	}
	version.Value = LocalRuntimeConfigAPIVersion
	migrated, err := yaml.Marshal(&document)
	if err != nil {
		return false, err
	}
	if err = replaceLocalConfigVersion(path, raw, migrated); err != nil {
		return false, err
	}
	policy := InitialLocalExecutionSecurity()
	if !seeded {
		policy = original.LocalRuntime.ExecutionSecurity
	}
	slog.Info("local execution-security configuration upgraded", "from", "donmai.dev/v1", "to", LocalRuntimeConfigAPIVersion, "seeded", seeded, "toolApproval", policy.ToolApproval, "fileRead", policy.FileRead, "fileWrite", policy.FileWrite, "network", policy.Network, "credentials", policy.Credentials, "isolation", policy.Isolation)
	return true, nil
}

func localYAMLKey(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
}

func localYAMLMember(mapping *yaml.Node, name string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func replaceLocalConfigVersion(path string, original, next []byte) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(absolute))
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	name := filepath.Base(absolute)
	info, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("local policy startup upgrade requires a regular configuration file")
	}
	temp, err := newLocalReference("config-upgrade-")
	if err != nil {
		return err
	}
	file, err := parent.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() { _ = parent.Remove(temp) }()
	written, writeErr := file.Write(next)
	if writeErr == nil && written != len(next) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	current, err := parent.ReadFile(name)
	if err != nil {
		return err
	}
	after, err := parent.Lstat(name)
	if err != nil {
		return err
	}
	if !os.SameFile(info, after) || !bytes.Equal(current, original) {
		return errors.New("configuration changed during local policy upgrade; no replacement was made")
	}
	if err = parent.Rename(temp, name); err != nil {
		return err
	}
	return syncLocalDirectory(parent)
}
