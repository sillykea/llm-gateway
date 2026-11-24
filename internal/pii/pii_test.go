package pii

import (
	"reflect"
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []Type
	}{
		{"clean", "summarise this paragraph about Go generics", []Type{}},
		{"email", "contact jane.doe@example.co.nz please", []Type{Email}},
		{"valid card", "card 4111 1111 1111 1111 exp 12/28", []Type{CreditCard}},
		{"invalid card is ignored", "order 4111 1111 1111 1112", []Type{}},
		{"nz phone", "call me on +64 21 555 0199", []Type{Phone}},
		{"ip", "server at 10.0.12.7 is down", []Type{IPAddress}},
		{"iban", "pay to FR76 3000 6000 0112 3456 7890 189", []Type{IBAN}},
		{"several", "mail a@b.io from 192.168.1.1", []Type{Email, IPAddress}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Detect(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Detect(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	out := Redact("email bob@example.com, card 4111111111111111")
	if strings.Contains(out, "bob@example.com") || strings.Contains(out, "4111111111111111") {
		t.Fatalf("personal data left in output: %q", out)
	}
	if !strings.Contains(out, "[EMAIL]") || !strings.Contains(out, "[CREDIT_CARD]") {
		t.Fatalf("placeholders missing: %q", out)
	}
}

func TestRedactKeepsSurroundingText(t *testing.T) {
	got := Redact("Hi, I'm Sam (sam@ex.io). Card: 4111-1111-1111-1111, thanks!")
	want := "Hi, I'm Sam ([EMAIL]). Card: [CREDIT_CARD], thanks!"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestNumbersInProseAreNotPII(t *testing.T) {
	for _, s := range []string{
		"we shipped 2024 release 3.2 to 1500 users",
		"order #48213977 arrived",
		"ISO 8601 timestamps",
	} {
		if got := Detect(s); len(got) != 0 {
			t.Errorf("Detect(%q) = %v, want nothing", s, got)
		}
	}
}
