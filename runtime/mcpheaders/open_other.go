//go:build !unix && !windows

package mcpheaders

import (
	"errors"
	"os"
)

var openBearerFile = openBearerFileNoFollow

func openBearerFileNoFollow(string) (*os.File, os.FileInfo, error) {
	return nil, nil, errors.New("secure MCP bearer file open is unsupported on this operating system")
}
