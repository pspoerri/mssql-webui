package main

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func fakeIDToken(payload map[string]any) string {
	b, _ := json.Marshal(payload)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(b) + ".sig"
}

func TestParseAndCheckClaims(t *testing.T) {
	c, err := parseIDToken(fakeIDToken(map[string]any{
		"aud": "cid", "tid": "t1", "name": "Ann", "preferred_username": "ann@example.com",
		"groups": []string{"g0", "g1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "Ann" || c.Email != "ann@example.com" {
		t.Fatalf("claims %+v", c)
	}
	if err := checkClaims(c, "cid", "t1", "g1"); err != nil {
		t.Fatalf("member rejected: %v", err)
	}
	if err := checkClaims(c, "cid", "t1", "g9"); err == nil {
		t.Fatal("non-member accepted")
	}
	if err := checkClaims(c, "other", "t1", "g1"); err == nil {
		t.Fatal("wrong audience accepted")
	}
	if err := checkClaims(c, "cid", "t2", "g1"); err == nil {
		t.Fatal("wrong tenant accepted")
	}
	if _, err := parseIDToken("nope"); err == nil {
		t.Fatal("malformed token accepted")
	}
}
