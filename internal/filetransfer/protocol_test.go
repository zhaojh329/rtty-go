package filetransfer

import (
	"strings"
	"testing"
)

func TestValidName(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../escape", "a/b", "a\x00b", strings.Repeat("x", 256)} {
		if ValidName([]byte(name)) {
			t.Errorf("accepted %q", name)
		}
	}
	for _, name := range []string{"file", ".hidden", "a\\b", strings.Repeat("x", 255)} {
		if !ValidName([]byte(name)) {
			t.Errorf("rejected %q", name)
		}
	}
}
