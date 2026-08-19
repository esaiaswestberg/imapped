//go:build live

package upstream_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// TestLiveSelectWireCapture records exactly what the server sends in response
// to SELECT and EXAMINE, to diagnose a parse failure.
//
// The raw protocol log necessarily contains the LOGIN command, so it is
// captured to a buffer and every line mentioning LOGIN is dropped before
// anything is printed.
func TestLiveSelectWireCapture(t *testing.T) {
	account := liveAccount(t)

	var wire bytes.Buffer
	client, err := imapclient.DialTLS(
		account.Host+":"+itoaPort(account.Port),
		&imapclient.Options{DebugWriter: &wire},
	)
	if err != nil {
		t.Fatalf("dialling: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = ctx

	if err := client.Login(account.Username, account.Password).Wait(); err != nil {
		t.Fatalf("signing in: %v", err)
	}

	// Discard everything captured so far: it contains the credentials.
	wire.Reset()

	t.Run("EXAMINE", func(t *testing.T) {
		_, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
		t.Logf("EXAMINE error: %v", err)
		t.Logf("wire:\n%s", redactWire(wire.String()))
		wire.Reset()
	})

	t.Run("SELECT", func(t *testing.T) {
		_, err := client.Select("INBOX", nil).Wait()
		t.Logf("SELECT error: %v", err)
		t.Logf("wire:\n%s", redactWire(wire.String()))
		wire.Reset()
	})
}

// redactWire removes any line that could carry a credential.
func redactWire(raw string) string {
	var kept []string
	for _, line := range strings.Split(raw, "\n") {
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "LOGIN") || strings.Contains(upper, "AUTHENTICATE") {
			kept = append(kept, "<redacted: authentication>")
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func itoaPort(p int) string {
	if p == 0 {
		return "993"
	}
	digits := ""
	for p > 0 {
		digits = string(rune('0'+p%10)) + digits
		p /= 10
	}
	return digits
}
