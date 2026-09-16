package server

import (
	"encoding/json"
	"errors"

	"github.com/RenseiAI/donmai/afclient/codeintel"
	"github.com/RenseiAI/donmai/internal/codeintelcontract"
)

// buildTools constructs the full six-tool set bound to this server's warm
// NativeRunner. Each tool mirrors 1:1 the corresponding `donmai code`
// subcommand handler in afcli/code.go — same options, same JSON output — so an
// agent that sees both surfaces gets consistent results. The enabled subset is
// selected from this set by registerTools.
//
// Tool results carry the SAME indented JSON the CLI prints (json.MarshalIndent
// with two-space indent, matching afcli's printJSON), wrapped in a single MCP
// text content item.
func (s *Server) buildTools() []*toolDef {
	r := s.runner
	root := s.root
	descriptors := make(map[string]codeintelcontract.Descriptor, 6)
	for _, descriptor := range codeintelcontract.Descriptors() {
		descriptors[descriptor.Name] = descriptor
	}

	return []*toolDef{
		{
			name:        ToolGetRepoMap,
			description: descriptors[ToolGetRepoMap].Description,
			inputSchema: descriptors[ToolGetRepoMap].InputSchema,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					MaxFiles     int      `json:"maxFiles"`
					FilePatterns []string `json:"filePatterns"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.GetRepoMapNative(codeintel.GetRepoMapOptions{
					MaxFiles:     in.MaxFiles,
					FilePatterns: in.FilePatterns,
				})
			},
		},
		{
			name:        ToolSearchSymbols,
			description: descriptors[ToolSearchSymbols].Description,
			inputSchema: descriptors[ToolSearchSymbols].InputSchema,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Query       string   `json:"query"`
					MaxResults  int      `json:"maxResults"`
					Kinds       []string `json:"kinds"`
					FilePattern string   `json:"filePattern"`
					IncludeDoc  bool     `json:"includeDoc"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.SearchSymbolsNative(codeintel.SearchSymbolsOptions{
					Query:       in.Query,
					MaxResults:  in.MaxResults,
					Kinds:       in.Kinds,
					FilePattern: in.FilePattern,
					IncludeDoc:  in.IncludeDoc,
				})
			},
		},
		{
			name:        ToolSearchCode,
			description: descriptors[ToolSearchCode].Description,
			inputSchema: descriptors[ToolSearchCode].InputSchema,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Query      string `json:"query"`
					MaxResults int    `json:"maxResults"`
					Language   string `json:"language"`
					IncludeDoc bool   `json:"includeDoc"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.SearchCodeNative(codeintel.SearchCodeOptions{
					Query:      in.Query,
					MaxResults: in.MaxResults,
					Language:   in.Language,
					IncludeDoc: in.IncludeDoc,
				})
			},
		},
		{
			name:        ToolCheckDuplicate,
			description: descriptors[ToolCheckDuplicate].Description,
			inputSchema: descriptors[ToolCheckDuplicate].InputSchema,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Content     string `json:"content"`
					ContentFile string `json:"contentFile"`
					MaxResults  int    `json:"maxResults"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				if (in.Content == "") == (in.ContentFile == "") {
					return nil, errors.New("exactly one of content or contentFile is required")
				}
				opts := codeintel.CheckDuplicateOptions{Content: in.Content, MaxResults: in.MaxResults}
				if in.ContentFile != "" {
					// Confine contentFile to the served root — reject absolute
					// paths and ../ escapes so a tool call cannot read outside
					// --root (the CLI allows arbitrary paths; the agent-facing
					// MCP surface does not).
					scoped, err := resolveScopedFile(root, in.ContentFile)
					if err != nil {
						return nil, err
					}
					opts.ContentFile = scoped
				}
				return r.CheckDuplicateNative(opts)
			},
		},
		{
			name:        ToolFindTypeUsages,
			description: descriptors[ToolFindTypeUsages].Description,
			inputSchema: descriptors[ToolFindTypeUsages].InputSchema,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					TypeName   string `json:"typeName"`
					MaxResults int    `json:"maxResults"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.FindTypeUsagesNative(codeintel.FindTypeUsagesOptions{
					TypeName:   in.TypeName,
					MaxResults: in.MaxResults,
				})
			},
		},
		{
			name:        ToolValidateCrossDeps,
			description: descriptors[ToolValidateCrossDeps].Description,
			inputSchema: descriptors[ToolValidateCrossDeps].InputSchema,
			invoke: func(args json.RawMessage) (any, error) {
				var in struct {
					Path string `json:"path"`
				}
				if err := decodeArgs(args, &in); err != nil {
					return nil, err
				}
				return r.ValidateCrossDepsNative(codeintel.ValidateCrossDepsOptions{Path: in.Path})
			},
		},
	}
}
