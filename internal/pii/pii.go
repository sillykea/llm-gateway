// Package pii finds personal data in text so the gateway can keep it away
// from third-party model providers.
//
// Detection is pattern-based and deliberately conservative: it is better to
// send a harmless request to a local model than to leak a real email address
// or card number to an external API.
//
// Performance: Go's regexp engine is linear-time but runs at only tens of
// MB/s, and it would be scanning every prompt. So the text is first cut into
// small candidate windows (the characters around each '@', and runs of
// digits) with a single byte scan, and the patterns only run on those.
// Ordinary prose drops out almost entirely. See BenchmarkDetect*.
package pii

import (
	"regexp"
	"sort"
	"strings"
)

// Type is a kind of personal data.
type Type string

const (
	Email      Type = "email"
	Phone      Type = "phone"
	CreditCard Type = "credit_card"
	IPAddress  Type = "ip_address"
	IBAN       Type = "iban"
)

type detector struct {
	typ     Type
	pattern *regexp.Regexp
	valid   func(string) bool // optional extra check to cut false positives
}

var (
	emailDetector = detector{typ: Email,
		pattern: regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)}

	// Order matters: when matches overlap, the earlier detector wins, so the
	// digits of a card number aren't reported again as a phone number.
	numberDetectors = []detector{
		{typ: CreditCard, valid: luhnValid,
			pattern: regexp.MustCompile(`\b(?:\d[ -]?){13,19}\b`)},
		{typ: IBAN,
			pattern: regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,3})?\b`)},
		{typ: IPAddress,
			pattern: regexp.MustCompile(`\b(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)\b`)},
		// International (+64 21 123 4567) or national numbers starting with 0
		// (021 123 4567). Requiring the + or 0 keeps order numbers out.
		{typ: Phone, valid: phoneValid,
			pattern: regexp.MustCompile(`(?:\+\d{1,3}|\b0)[ -]?\(?\d{1,4}\)?[ -]?\d{3,4}[ -]?\d{3,4}\b`)},
	}
)

// match is a piece of personal data at text[start:end].
type match struct {
	start, end int
	typ        Type
}

// Detect returns the distinct kinds of personal data found in text, sorted.
func Detect(text string) []Type {
	seen := map[Type]bool{}
	for _, m := range find(text) {
		seen[m.typ] = true
	}
	types := make([]Type, 0, len(seen))
	for t := range seen {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
	return types
}

// Redact replaces personal data with placeholders such as [EMAIL].
func Redact(text string) string {
	matches := find(text)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m.start])
		b.WriteString("[" + strings.ToUpper(string(m.typ)) + "]")
		last = m.end
	}
	b.WriteString(text[last:])
	return b.String()
}

// find returns non-overlapping matches in text order.
func find(text string) []match {
	var out []match

	for _, w := range emailWindows(text) {
		out = appendMatches(out, text, w, emailDetector)
	}
	for _, w := range numberWindows(text) {
		for _, d := range numberDetectors {
			out = appendMatches(out, text, w, d)
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// appendMatches runs d over text[w.start:w.end] and keeps valid matches that
// don't overlap one already found.
func appendMatches(out []match, text string, w window, d detector) []match {
	for _, loc := range d.pattern.FindAllStringIndex(text[w.start:w.end], -1) {
		m := match{start: w.start + loc[0], end: w.start + loc[1], typ: d.typ}
		if d.valid != nil && !d.valid(text[m.start:m.end]) {
			continue
		}
		if overlaps(out, m) {
			continue
		}
		out = append(out, m)
	}
	return out
}

func overlaps(existing []match, m match) bool {
	for _, e := range existing {
		if m.start < e.end && e.start < m.end {
			return true
		}
	}
	return false
}

type window struct{ start, end int }

func isEmailChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '%' || c == '+' || c == '-'
}

// emailWindows returns the run of address characters around each '@'.
func emailWindows(text string) []window {
	var out []window
	for i := 0; i < len(text); i++ {
		if text[i] != '@' {
			continue
		}
		s, e := i, i+1
		for s > 0 && isEmailChar(text[s-1]) {
			s--
		}
		for e < len(text) && isEmailChar(text[e]) {
			e++
		}
		out = append(out, window{s, e})
		i = e - 1
	}
	return out
}

// isNumberChar covers everything the number patterns can contain. Lowercase
// letters are excluded, so ordinary words split the text into tiny pieces.
func isNumberChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' ||
		c == ' ' || c == '-' || c == '+' || c == '(' || c == ')' || c == '.'
}

// numberWindows returns runs of number characters holding at least 4 digits,
// the fewest any number pattern needs (an IP address like 1.1.1.1).
func numberWindows(text string) []window {
	var out []window
	for i := 0; i < len(text); {
		if !isNumberChar(text[i]) {
			i++
			continue
		}
		s, digits := i, 0
		for i < len(text) && isNumberChar(text[i]) {
			if text[i] >= '0' && text[i] <= '9' {
				digits++
			}
			i++
		}
		if digits >= 4 {
			out = append(out, window{s, i})
		}
	}
	return out
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// luhnValid applies the Luhn checksum used by every major card network, which
// stops long order numbers or timestamps being flagged as card numbers.
func luhnValid(s string) bool {
	digits := digitsOnly(s)
	if len(digits) < 13 || len(digits) > 19 {
		return false
	}
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		n := int(digits[i] - '0')
		if double {
			n *= 2
			if n > 9 {
				n -= 9
			}
		}
		sum += n
		double = !double
	}
	return sum%10 == 0
}

func phoneValid(s string) bool {
	n := len(digitsOnly(s))
	return n >= 8 && n <= 15
}
