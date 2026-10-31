package token

import "testing"

func TestTokenFitsTelegram(t *testing.T) {
	got := NewToken()
	if len(got) != 22 {
		t.Fatalf("len %d", len(got))
	}
	if Hash("dev-key") == Hash("other") {
		t.Fatal("hash")
	}
}
