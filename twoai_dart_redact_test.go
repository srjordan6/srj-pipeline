package main

import (
	"errors"
	"strings"
	"testing"
)

func TestDartRedact(t *testing.T) {
	err := errors.New(`Get "https://opendart.fss.or.kr/api/corpCode.xml?crtfc_key=abc123secret": remote error: tls: handshake failure`)
	got := dartRedact(err, "abc123secret")
	if strings.Contains(got, "abc123secret") || !strings.Contains(got, "crtfc_key=REDACTED") {
		t.Errorf("key not redacted: %s", got)
	}
}
