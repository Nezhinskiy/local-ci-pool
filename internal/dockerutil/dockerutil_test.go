package dockerutil

import (
	"strings"
	"testing"
)

func TestDrainStream(t *testing.T) {
	ok := `{"stream":"Step 1/2 : FROM scratch\n"}` + "\n" + `{"status":"Pulling"}` + "\n" + `{"aux":{"ID":"sha256:abc"}}` + "\n"
	if err := DrainStream(strings.NewReader(ok)); err != nil {
		t.Fatalf("a clean stream: %v", err)
	}
	if err := DrainStream(strings.NewReader("")); err != nil {
		t.Fatalf("an empty stream: %v", err)
	}

	failed := `{"stream":"Step 1/2 : RUN false\n"}` + "\n" + `{"stream":"boom\n"}` + "\n" +
		`{"errorDetail":{"code":1,"message":"The command returned a non-zero code: 1"},"error":"The command returned a non-zero code: 1"}` + "\n"
	err := DrainStream(strings.NewReader(failed))
	if err == nil {
		t.Fatal("an error message inside a 200 stream must be an error")
	}
	for _, want := range []string{"non-zero code: 1", "boom", "RUN false"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}

	if err := DrainStream(strings.NewReader(`{"stream":"x"} garbage`)); err == nil {
		t.Fatal("a corrupt stream must be an error")
	}
}

func TestDrainStreamKeepsOnlyTheLastLines(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(`{"stream":"line ` + string(rune('a'+i%26)) + string(rune('0'+i/26)) + `\n"}` + "\n")
	}
	b.WriteString(`{"error":"failed"}` + "\n")
	err := DrainStream(strings.NewReader(b.String()))
	if err == nil {
		t.Fatal("want an error")
	}
	if got := strings.Count(err.Error(), "line "); got != tailLines {
		t.Fatalf("error carries %d output lines, want %d:\n%v", got, tailLines, err)
	}
}
