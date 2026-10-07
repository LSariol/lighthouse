package deploy

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// minSecretLength is the shortest secret value that's hidden in output.
// Hiding shorter ones would garble ordinary words and numbers.
const minSecretLength = 4

// hidden replaces a secret value in output.
const hidden = "[secret]"

// scrubber writes complete lines with every secret value hidden.
type scrubber struct {
	mu      sync.Mutex
	out     []io.Writer
	secrets []string
	partial []byte
}

func newScrubber(secrets map[string]string, out ...io.Writer) *scrubber {
	s := &scrubber{out: out}
	for _, v := range secrets {
		if len(v) >= minSecretLength {
			s.secrets = append(s.secrets, v)
		}
	}
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
	return s
}

// add hides more values from now on.
func (s *scrubber) add(secrets map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range secrets {
		if len(v) >= minSecretLength {
			s.secrets = append(s.secrets, v)
		}
	}
	sort.Slice(s.secrets, func(i, j int) bool { return len(s.secrets[i]) > len(s.secrets[j]) })
}

func (s *scrubber) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.partial = append(s.partial, p...)
	if i := bytes.LastIndexByte(s.partial, '\n'); i >= 0 {
		s.emit(s.partial[:i+1])
		s.partial = append(s.partial[:0], s.partial[i+1:]...)
	}
	return len(p), nil
}

// Flush writes out a partial last line.
func (s *scrubber) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.partial) > 0 {
		s.emit(s.partial)
		s.partial = s.partial[:0]
	}
}

func (s *scrubber) emit(b []byte) {
	text := s.scrub(string(b))
	for _, w := range s.out {
		io.WriteString(w, text)
	}
}

func (s *scrubber) scrub(text string) string {
	for _, v := range s.secrets {
		text = strings.ReplaceAll(text, v, hidden)
	}
	return text
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTail(max int) *tail { return &tail{max: max} }

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
