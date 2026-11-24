package pii

import (
	"strings"
	"testing"
)

// A typical prompt is a few kilobytes of ordinary text with nothing to find,
// which is the case the scanner has to be fast for.
var prompt = strings.Repeat("Please summarise the attached quarterly report and list the three biggest risks. ", 50)

func BenchmarkDetectClean(b *testing.B) {
	b.SetBytes(int64(len(prompt)))
	for i := 0; i < b.N; i++ {
		Detect(prompt)
	}
}

func BenchmarkDetectWithPII(b *testing.B) {
	text := prompt + " reply to jane@example.com or +64 21 555 0199"
	b.SetBytes(int64(len(text)))
	for i := 0; i < b.N; i++ {
		Detect(text)
	}
}
