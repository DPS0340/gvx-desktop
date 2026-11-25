package keychain

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A test binary never reaches the user's keychain, whatever the system
// has: code under test is handed a Fake.
func TestSystemIsNoneInATest(t *testing.T) {
	kc, err := System()
	if kc != nil || err == nil {
		t.Fatalf("System() = %v, %v in a test", kc, err)
	}
	t.Setenv(EnvVar, "file")
	kc, err = System()
	if kc != nil || err == nil {
		t.Fatalf("System() with %s=file = %v, %v in a test", EnvVar, kc, err)
	}
}

func TestSystemWithEnvAndProbe(t *testing.T) {
	called := false
	errProbe := func() (Keychain, error) {
		called = true
		return nil, errors.New("probe failed")
	}

	// Test guard takes precedence even when env="file".
	if kc, err := systemWith(true, "file", errProbe); kc != nil || err == nil || called {
		t.Fatalf("systemWith(true, file) = %v, %v (called=%v)", kc, err, called)
	}

	// GVX_KEYCHAIN=file disables OS keychain without probing.
	for _, v := range []string{"file", "FILE", "File"} {
		called = false
		kc, err := systemWith(false, v, errProbe)
		if kc != nil || err == nil || !strings.Contains(err.Error(), EnvVar+"=file") || called {
			t.Fatalf("systemWith(false, %q) = %v, %v (called=%v)", v, kc, err, called)
		}
	}

	// Probe errors propagate.
	called = false
	if kc, err := systemWith(false, "", errProbe); kc != nil || err == nil || !called || !strings.Contains(err.Error(), "probe failed") {
		t.Fatalf("systemWith(false, '', errProbe) = %v, %v (called=%v)", kc, err, called)
	}

	// Successful probe returns a timed-wrapped Keychain.
	var fake Fake
	kc, err := systemWith(false, "", func() (Keychain, error) {
		return &fake, nil
	})
	if err != nil || kc == nil {
		t.Fatalf("systemWith(false, '', ok) = %v, %v", kc, err)
	}
	if kc.Name() != fake.Name() {
		t.Fatalf("wrapped Name() = %q, want %q", kc.Name(), fake.Name())
	}
	if err := kc.Set("svc", "acc", "secret"); err != nil {
		t.Fatalf("wrapped Set: %v", err)
	}
	got, err := kc.Get("svc", "acc")
	if err != nil || got != "secret" {
		t.Fatalf("wrapped Get = %q, %v", got, err)
	}
}

func TestTimedDelegationAndTimeout(t *testing.T) {
	var fake Fake
	kc := timed{k: &fake, timeout: 200 * time.Millisecond}

	if kc.Name() != "test keychain" {
		t.Fatalf("Name() = %q", kc.Name())
	}
	if _, err := kc.Get("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
	if err := kc.Set("s", "a", "val"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, err := kc.Get("s", "a"); err != nil || got != "val" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := kc.Delete("s", "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := kc.Delete("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete = %v, want ErrNotFound", err)
	}

	// Non-timeout error propagates through timed.
	wantErr := errors.New("boom")
	fake.Fail = wantErr
	if err := kc.Set("s", "a", "x"); !errors.Is(err, wantErr) {
		t.Fatalf("Set with Fail = %v, want %v", err, wantErr)
	}

	// Slow backend triggers ErrTimeout without waiting 20s.
	block := make(chan struct{})
	defer close(block)
	slow := timed{k: slowKeychain{block: block}, timeout: 20 * time.Millisecond}

	if _, err := slow.Get("s", "a"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("slow Get = %v, want ErrTimeout", err)
	}
	if err := slow.Set("s", "a", "v"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("slow Set = %v, want ErrTimeout", err)
	}
	if err := slow.Delete("s", "a"); !errors.Is(err, ErrTimeout) {
		t.Fatalf("slow Delete = %v, want ErrTimeout", err)
	}
}

type slowKeychain struct {
	block <-chan struct{}
}

func (slowKeychain) Name() string { return "slow" }

func (s slowKeychain) Get(string, string) (string, error) {
	<-s.block
	return "", nil
}

func (s slowKeychain) Set(string, string, string) error {
	<-s.block
	return nil
}

func (s slowKeychain) Delete(string, string) error {
	<-s.block
	return nil
}

func TestFake(t *testing.T) {
	var f Fake
	if f.Name() != "test keychain" {
		t.Fatalf("Name = %q", f.Name())
	}
	if _, err := f.Get("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty Get: %v", err)
	}
	if err := f.Set("s", "a", "v"); err != nil {
		t.Fatal(err)
	}
	if v, err := f.Get("s", "a"); err != nil || v != "v" {
		t.Fatalf("Get = %q, %v", v, err)
	}
	// Empty value and overwrite.
	if err := f.Set("s", "a", ""); err != nil {
		t.Fatal(err)
	}
	if v, err := f.Get("s", "a"); err != nil || v != "" {
		t.Fatalf("empty value Get = %q, %v", v, err)
	}
	if err := f.Set("b", "z", "1"); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("a", "b", "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("s", "b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account: %v", err)
	}

	gotEntries := f.Entries()
	wantEntries := [][2]string{{"a", "b"}, {"b", "z"}, {"s", "a"}}
	if !slices.Equal(gotEntries, wantEntries) {
		t.Fatalf("Entries() = %v, want %v", gotEntries, wantEntries)
	}

	if err := f.Delete("s", "a"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("s", "a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second Delete: %v", err)
	}
	if got := f.Entries(); !slices.Equal(got, [][2]string{{"a", "b"}, {"b", "z"}}) {
		t.Fatalf("Entries() after delete = %v", got)
	}
	if f.Calls != 10 {
		t.Fatalf("Calls = %d, want 10", f.Calls)
	}

	f.Fail = errors.New("locked")
	if _, err := f.Get("a", "b"); !errors.Is(err, f.Fail) {
		t.Fatalf("Get with Fail = %v", err)
	}
	if err := f.Set("s", "a", "v"); !errors.Is(err, f.Fail) {
		t.Fatalf("Set with Fail = %v", err)
	}
	if err := f.Delete("a", "b"); !errors.Is(err, f.Fail) {
		t.Fatalf("Delete with Fail = %v", err)
	}
	if f.Calls != 13 {
		t.Fatalf("Calls after Fail = %d, want 13", f.Calls)
	}
}

func TestFakeConcurrent(t *testing.T) {
	var f Fake
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 25 {
				svc := fmt.Sprintf("svc-%d", id)
				acc := fmt.Sprintf("acc-%d", i)
				_ = f.Set(svc, acc, "secret")
				_, _ = f.Get(svc, acc)
				_ = f.Entries()
				if i%2 == 0 {
					_ = f.Delete(svc, acc)
				}
			}
		}(g)
	}
	wg.Wait()
	if f.Calls == 0 {
		t.Fatal("expected non-zero Calls")
	}
}
