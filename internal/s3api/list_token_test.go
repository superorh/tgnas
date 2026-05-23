package s3api

import "testing"

func TestContinuationTokenRoundTrip(t *testing.T) {
	want := "photos/2026/kitten.jpg"

	token, err := EncodeContinuationToken(want)
	if err != nil {
		t.Fatalf("EncodeContinuationToken returned error: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}

	got, err := DecodeContinuationToken(token)
	if err != nil {
		t.Fatalf("DecodeContinuationToken returned error: %v", err)
	}
	if got != want {
		t.Fatalf("decoded key = %q, want %q", got, want)
	}
}

func TestDecodeContinuationTokenAcceptsPresentEmptyLastKey(t *testing.T) {
	token := "eyJsYXN0X2tleSI6IiJ9"

	lastKey, err := DecodeContinuationToken(token)
	if err != nil {
		t.Fatalf("DecodeContinuationToken returned error: %v", err)
	}
	if lastKey != "" {
		t.Fatalf("lastKey = %q, want empty", lastKey)
	}
}

func TestDecodeContinuationTokenRejectsInvalid(t *testing.T) {
	cases := []string{"not-base64!", "e30", "eyJ1bmV4cGVjdGVkIjoieCJ9", "eyJsYXN0X2tleSI6MX0"}

	for _, token := range cases {
		t.Run(token, func(t *testing.T) {
			if _, err := DecodeContinuationToken(token); err == nil {
				t.Fatalf("DecodeContinuationToken(%q) error = nil, want non-nil", token)
			}
		})
	}
}
