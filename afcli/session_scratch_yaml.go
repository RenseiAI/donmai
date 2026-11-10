package afcli

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"

	daemonRuntime "github.com/RenseiAI/donmai/daemon"
)

// writeSessionScratchDerive records the sessionScratch.derive setting in
// daemon.yaml with a raw-node edit, so every other key — including keys the
// daemon's own config model does not declare — is preserved. The value is
// always written explicitly, on or off, so the file states the setting
// rather than leaving it to the default.
func writeSessionScratchDerive(path string, derive bool) error {
	doc, isNew, err := loadUsagePingDocForWrite(path)
	if err != nil {
		return err
	}
	block := findMappingValue(doc, "sessionScratch")
	if block == nil {
		block = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		doc.Content = append(doc.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "sessionScratch"},
			block,
		)
	}
	if block.Kind != yaml.MappingNode {
		return fmt.Errorf("daemon config %q: sessionScratch is not a mapping; fix it by hand before setting %s", path, daemonRuntime.SessionScratchDeriveKey)
	}
	upsertUsagePingKey(block, "derive", &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(derive)})
	return storeUsagePingDoc(path, doc, isNew)
}
