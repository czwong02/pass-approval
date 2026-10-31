package token

import "testing"

func TestTokenFitsTelegram(t *testing.T) {
	got := NewToken()
	if len(got) != 22 {
		t.Fatalf("NewToken() = %q, want 22 characters", got)
	}
	if Hash("dev-key") == Hash("other") {
		t.Fatalf("Hash(\"dev-key\") == Hash(\"other\")")
	}
}
