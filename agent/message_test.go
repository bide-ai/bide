package agent

import (
	"bytes"
	"encoding/json"
	"testing"
)

// An Image part (both the raw-bytes and URL variants) survives the type-tagged JSON
// round-trip that durable stores rely on.
func TestImagePart_RoundTrips(t *testing.T) {
	raw := []byte{0x89, 0x50, 0x4e, 0x47} // arbitrary bytes
	msg := UserParts(
		Text{"what is this?"},
		ImageData("image/png", raw),
		ImageURL("https://example.com/cat.jpg"),
	)

	b, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Message
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Role != RoleUser {
		t.Errorf("role = %q, want user", got.Role)
	}
	if len(got.Parts) != 3 {
		t.Fatalf("parts = %d, want 3", len(got.Parts))
	}
	if txt, ok := got.Parts[0].(Text); !ok || txt.Text != "what is this?" {
		t.Errorf("part[0] = %+v, want text", got.Parts[0])
	}
	img, ok := got.Parts[1].(Image)
	if !ok {
		t.Fatalf("part[1] type = %T, want Image", got.Parts[1])
	}
	if img.Mime != "image/png" || !bytes.Equal(img.Data, raw) || img.URL != "" {
		t.Errorf("bytes image round-trip = %+v", img)
	}
	urlImg, ok := got.Parts[2].(Image)
	if !ok {
		t.Fatalf("part[2] type = %T, want Image", got.Parts[2])
	}
	if urlImg.URL != "https://example.com/cat.jpg" || len(urlImg.Data) != 0 {
		t.Errorf("url image round-trip = %+v", urlImg)
	}
}
