package terminal

import (
	"os"
	"testing"
	"time"
)

// newAwokenInput is a seamlessInput reading keys from a pipe, waking on wake.
func newAwokenInput(t *testing.T, wake chan struct{}, typed string) (*seamlessInput, *os.File) {
	t.Helper()
	keys, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		keys.Close()
		w.Close()
	})
	pipe, err := newWakePipe(wake)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pipe.Close(); err != nil {
			t.Error(err)
		}
	})
	s := &seamlessInput{fd: int(keys.Fd()), ed: NewPromptEditor(""), wake: wake, wakeFd: pipe.r}
	s.ed.runes = []rune(typed)
	return s, w
}

// awokenWithin runs s.awoken and fails the test if it does not return within a second.
func awokenWithin(t *testing.T, s *seamlessInput) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- s.awoken() }()
	select {
	case woke := <-done:
		return woke
	case <-time.After(time.Second):
		t.Fatal("awoken did not return")
		return false
	}
}

func TestAwokenEndsAnEmptyPromptWhenWakeCloses(t *testing.T) {
	wake := make(chan struct{})
	s, _ := newAwokenInput(t, wake, "")
	time.AfterFunc(20*time.Millisecond, func() { close(wake) })
	if !awokenWithin(t, s) {
		t.Error("awoken = false after wake closed on an empty prompt")
	}
}

func TestAwokenReturnsForAKey(t *testing.T) {
	s, w := newAwokenInput(t, make(chan struct{}), "")
	if _, err := w.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	if awokenWithin(t, s) {
		t.Error("awoken = true for a key")
	}
	if s.wake == nil {
		t.Error("a key dropped the wake channel")
	}
}

func TestAwokenKeepsTypedText(t *testing.T) {
	wake := make(chan struct{})
	close(wake)
	s, _ := newAwokenInput(t, wake, "draft")
	if awokenWithin(t, s) {
		t.Error("awoken = true over typed text")
	}
	if s.wake != nil {
		t.Error("the wake channel is kept after it fired over typed text")
	}
	if s.awoken() {
		t.Error("awoken = true once the wake was dropped")
	}
}

func TestWakePipeClosesWithoutAWake(t *testing.T) {
	pipe, err := newWakePipe(make(chan struct{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := pipe.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
}
