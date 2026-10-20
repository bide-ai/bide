package notify

import (
	"testing"
	"time"

	"github.com/bide-ai/bide/internal/safefile/safefiletest"
)

// The certificate, key and CA files a webhook names are read without waiting on a named pipe, and a
// directory in their place is refused.
func TestFiles_NamedPipeAndDirectory(t *testing.T) {
	p := safefiletest.FIFO(t)
	d := t.TempDir()
	safefiletest.Deadline(t, "notify readers", func() {
		if _, err := newClientCertFiles(p, p, time.Now, func(error) {}); err == nil {
			t.Error("newClientCertFiles read a named pipe")
		}
		if err := WithServerCA(p)(&Webhook{}); err == nil {
			t.Error("WithServerCA read a named pipe")
		}
	})
	if _, err := newClientCertFiles(d, d, time.Now, func(error) {}); err == nil {
		t.Error("newClientCertFiles read a directory")
	}
	if err := WithServerCA(d)(&Webhook{}); err == nil {
		t.Error("WithServerCA read a directory")
	}
}
