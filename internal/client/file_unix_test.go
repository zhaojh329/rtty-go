//go:build !windows

package client

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/zhaojh329/rtty-go/internal/filetransfer"
)

func TestDownloadRejectsInvalidFilename(t *testing.T) {
	for _, name := range []string{"../escaped", "nested/name", ".", "..", ""} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "downloads")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}

			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()

			ctx := &RttyFileContext{savepath: directory, fifo: writer}
			ctx.startDownload(append([]byte{0, 0, 0, 1}, name...))

			msg := make([]byte, filetransfer.ControlMessageSize)
			if _, err := io.ReadFull(reader, msg); err != nil || msg[0] != filetransfer.ControlErr {
				t.Fatalf("invalid name %q: response=%v error=%v", name, msg[0], err)
			}
			if ctx.savepath != directory || ctx.file != nil {
				t.Fatalf("invalid name %q changed destination", name)
			}
		})
	}
}
