package ledger

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Ledger {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ledger.db")
	l, err := Open(p)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestAddAccumulatesToday(t *testing.T) {
	l := openTemp(t)
	if _, err := l.Add("/proj", 100, 50, 0.10); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Add("/proj", 200, 80, 0.20); err != nil {
		t.Fatal(err)
	}
	got, err := l.Today("/proj")
	if err != nil {
		t.Fatal(err)
	}
	if got.TokensIn != 300 || got.TokensOut != 130 || got.Requests != 2 {
		t.Fatalf("unexpected entry: %+v", got)
	}
	if got.USD < 0.299 || got.USD > 0.301 {
		t.Fatalf("USD not summed: %v", got.USD)
	}
}

func TestProjectTotalIsolatesByProject(t *testing.T) {
	l := openTemp(t)
	if _, err := l.Add("/a", 10, 5, 0.05); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Add("/b", 99, 99, 9.99); err != nil {
		t.Fatal(err)
	}
	a, _ := l.ProjectTotal("/a")
	if a.Requests != 1 || a.USD < 0.049 || a.USD > 0.051 {
		t.Fatalf("project /a leaked: %+v", a)
	}
}
