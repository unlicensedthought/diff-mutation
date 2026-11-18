package hostload

import (
	"errors"
	"testing"
)

// Bugs named: an oversubscribed host admitted; an unreadable load read as 0
// (admitting the run on a host we could not measure).
func TestCheck(t *testing.T) {
	at := func(v float64) func() (float64, error) { return func() (float64, error) { return v, nil } }
	if err := Check(at(8.0), 8); err != nil {
		t.Fatalf("load == cores refused: %v", err)
	}
	if err := Check(at(8.01), 8); err == nil {
		t.Fatal("load > cores admitted")
	}
	if err := Check(func() (float64, error) { return 0, errors.New("no") }, 8); err == nil {
		t.Fatal("unreadable load admitted")
	}
	if _, err := Load1(); err != nil {
		t.Fatalf("Load1 on this host: %v", err)
	}
}
