package agentmcp

import (
	"bytes"
	"io"
	"os"
)

func removeFile(path string) error {
	return os.Remove(path)
}

func bytesReader(b []byte) io.Reader {
	return bytes.NewReader(b)
}
