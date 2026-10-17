package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spore-host/truffle/pkg/find"
)

// A query of only unknown words is refused by truffle (truffle#229) and must
// reach the user as truffle's own message, which names the words and what find
// understands, not wrapped as "parse query: ..." (#56). It is refused before any
// AWS call, so this needs no credentials.
func TestHandleTruffleFind_UnrecognizedQueryIsTruffleMessage(t *testing.T) {
	_, perr := find.ParseQuery("zzqx")
	var uq *find.UnrecognizedQueryError
	if !errors.As(perr, &uq) {
		t.Fatalf("ParseQuery(%q) = %v, want *find.UnrecognizedQueryError", "zzqx", perr)
	}
	res, err := handleTruffleFind(context.Background(), newRequest(map[string]any{"query": "zzqx"}))
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("want an error result, got %+v", res)
	}
	if got := resultText(res); got != uq.Error() {
		t.Errorf("result = %q, want truffle's message %q", got, uq.Error())
	}
}

// A query that ran but ignored a word says so, as truffle's CLI does.
func TestFindQueryNotes(t *testing.T) {
	pq, err := find.ParseQuery("gpu zzqx")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if got := findQueryNotes(pq); !strings.Contains(got, "ignored unknown term(s): zzqx") {
		t.Errorf("notes = %q, want the ignored word named", got)
	}
	pq, err = find.ParseQuery("gpu")
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	if got := findQueryNotes(pq); got != "" {
		t.Errorf("notes for a fully recognised query = %q, want none", got)
	}
}
