// Copyright 2026 BareProxy.com
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// BareProxy's own functions, which any Proxy-Wasm SDK reaches through
// proxy_call_foreign_function:
//
//	bareproxy_note        args: text. A line for explain, why and the record.
//	bareproxy_store_get   args: key. Returns the value, or NOT_FOUND.
//	bareproxy_store_put   args: 4-byte little-endian key length, key, value.
//	bareproxy_store_delete args: key.
//	bareproxy_read_file   args: a path inside a folder the config names with
//	                      read. Returns the file, or NOT_FOUND.
func foreign(c *hcall, a []uint64) uint32 {
	name, ok1 := c.str(a[0], a[1])
	args, ok2 := c.read(a[2], a[3])
	if !ok1 || !ok2 {
		return stMemory
	}
	p := c.in.p
	switch name {
	case "bareproxy_note":
		note := strings.TrimSpace(string(args))
		if len(note) > 500 {
			note = note[:500]
		}
		if s := c.stream(); s != nil {
			if len(s.notes) < 20 {
				s.notes = append(s.notes, note)
			}
		} else {
			p.logf("%s", note)
		}
		return c.give(nil, a[4], a[5])
	case "bareproxy_store_get", "bareproxy_store_put", "bareproxy_store_delete":
		if p.store == nil {
			return stNotFound
		}
		var err error
		var val []byte
		switch name {
		case "bareproxy_store_get":
			val, err = p.store.get(string(args))
		case "bareproxy_store_delete":
			err = p.store.delete(string(args))
		default:
			if len(args) < 4 || int(binary.LittleEndian.Uint32(args)) > len(args)-4 {
				return stBadArgument
			}
			kl := int(binary.LittleEndian.Uint32(args))
			err = p.store.put(string(args[4:4+kl]), args[4+kl:])
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return stNotFound
		case errors.Is(err, errStoreFull) || errors.Is(err, errBadKey):
			return stBadArgument
		case err != nil:
			return stInternal
		}
		return c.give(val, a[4], a[5])
	case "bareproxy_read_file":
		rel := strings.TrimPrefix(filepath.ToSlash(string(args)), "/")
		for _, root := range p.S.Read {
			f, err := root.Open(rel)
			if err != nil {
				continue
			}
			if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() { // a pipe would block
				f.Close()
				continue
			}
			b, err := io.ReadAll(io.LimitReader(f, max(p.S.BodyMax, 1<<20)+1))
			f.Close()
			if err != nil {
				return stInternal
			}
			if int64(len(b)) > max(p.S.BodyMax, 1<<20) {
				return stBadArgument
			}
			return c.give(b, a[4], a[5])
		}
		return stNotFound
	}
	return stNotFound
}

var (
	errStoreFull = errors.New("the store is full")
	errBadKey    = errors.New("a key must be 1 to 1,024 bytes")
)

// store is a plugin's key-value store on disk: one file per key, named by
// the key's SHA-256, under a size cap that counts 4 KB for each file besides
// its bytes, so many small keys can't fill the disk either.
const fileCost = 4 << 10

type store struct {
	mu   sync.Mutex
	dir  string
	max  int64
	used int64
}

func openStore(dir string, max int64) (*store, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	st := &store{dir: dir, max: max}
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				st.used += fi.Size() + fileCost
			}
		}
		return nil
	})
	return st, err
}

func (st *store) file(key string) (string, error) {
	if len(key) == 0 || len(key) > 1024 {
		return "", errBadKey
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(st.dir, hex.EncodeToString(sum[:])), nil
}

func (st *store) get(key string) ([]byte, error) {
	f, err := st.file(key)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(f)
}

func (st *store) put(key string, val []byte) error {
	f, err := st.file(key)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var old int64
	if fi, err := os.Stat(f); err == nil {
		old = fi.Size() + fileCost
	}
	if st.used-old+int64(len(val))+fileCost > st.max {
		return errStoreFull
	}
	tmp := f + ".tmp"
	if err := os.WriteFile(tmp, val, 0o640); err != nil {
		return err
	}
	if err := os.Rename(tmp, f); err != nil {
		os.Remove(tmp)
		return err
	}
	st.used += int64(len(val)) + fileCost - old
	return nil
}

func (st *store) delete(key string) error {
	f, err := st.file(key)
	if err != nil {
		return err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	fi, err := os.Stat(f)
	if err != nil {
		return err
	}
	if err := os.Remove(f); err != nil {
		return err
	}
	st.used -= fi.Size() + fileCost
	return nil
}
