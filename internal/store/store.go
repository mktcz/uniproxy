package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Store {
	return &Store{path: filepath.Clean(path)}
}

func (s *Store) Path() string { return s.path }

func (s *Store) Get(provider string, dest any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		return err
	}
	raw, ok := doc[provider]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return os.ErrNotExist
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("decode %s credentials: %w", provider, err)
	}
	return nil
}

func (s *Store) Put(provider string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		doc = map[string]json.RawMessage{}
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode %s credentials: %w", provider, err)
	}
	doc[provider] = raw
	if err := s.save(doc); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func (s *Store) Delete(provider string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doc, err := s.load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, ok := doc[provider]; !ok {
		return nil
	}
	delete(doc, provider)
	if err := s.save(doc); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

func (s *Store) load() (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("read secure JSON file: %w", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode secure JSON file: %w", err)
	}
	if doc == nil {
		doc = map[string]json.RawMessage{}
	}
	return doc, nil
}

func (s *Store) save(doc map[string]json.RawMessage) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create secure JSON directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure JSON directory: %w", err)
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secure JSON file: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".secure-json-*")
	if err != nil {
		return fmt.Errorf("create temporary secure JSON file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary JSON file: %w", err)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write temporary JSON file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary JSON file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary JSON file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("replace secure JSON file: %w", err)
	}
	ok = true
	return nil
}
